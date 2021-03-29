package destinations

import (
	"errors"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"github.com/jitsucom/jitsu/server/appconfig"
	"github.com/jitsucom/jitsu/server/events"
	"github.com/jitsucom/jitsu/server/logging"
	"github.com/jitsucom/jitsu/server/resources"
	"github.com/jitsucom/jitsu/server/storages"
	"github.com/spf13/viper"
	"strings"
	"sync"
	"time"
)

const serviceName = "destinations"
const marshallingErrorMsg = `Error initializing destinations: wrong config format: each destination must contains one key and config as a value(see https://docs.eventnative.dev/configuration) e.g. 
destinations:  
  custom_name:
    type: redshift
    ...
`

//LoggerUsage is used for counting when logger isn't used
type LoggerUsage struct {
	logger events.Consumer
	usage  int
}

//Service is reloadable service of events destinations per token
type Service struct {
	storageFactory storages.Factory
	loggerFactory  *logging.Factory

	//map for holding all destinations for closing
	unitsByName map[string]*Unit
	//map for holding all loggers for closing
	loggersUsageByTokenID map[string]*LoggerUsage

	sync.RWMutex
	consumersByTokenID      TokenizedConsumers
	storagesByTokenID       TokenizedStorages
	destinationsIDByTokenID TokenizedIDs
}

//only for tests
func NewTestService(consumersByTokenID TokenizedConsumers, storagesByTokenID TokenizedStorages, destinationsIDByTokenID TokenizedIDs) *Service {
	return &Service{
		consumersByTokenID:      consumersByTokenID,
		storagesByTokenID:       storagesByTokenID,
		destinationsIDByTokenID: destinationsIDByTokenID,
	}
}

//NewService return loaded Service instance and call resources.Watcher() if destinations source is http url or file path
func NewService(destinations *viper.Viper, destinationsSource string, storageFactory storages.Factory, loggerFactory *logging.Factory) (*Service, error) {
	service := &Service{
		storageFactory: storageFactory,
		loggerFactory:  loggerFactory,

		unitsByName:           map[string]*Unit{},
		loggersUsageByTokenID: map[string]*LoggerUsage{},

		consumersByTokenID:      map[string]map[string]events.Consumer{},
		storagesByTokenID:       map[string]map[string]storages.StorageProxy{},
		destinationsIDByTokenID: map[string]map[string]bool{},
	}

	reloadSec := viper.GetInt("server.destinations_reload_sec")
	if reloadSec == 0 {
		return nil, errors.New("server.destinations_reload_sec can't be empty")
	}

	if destinations != nil {
		dc := map[string]storages.DestinationConfig{}
		if err := destinations.Unmarshal(&dc); err != nil {
			logging.Error(marshallingErrorMsg, err)
			return service, nil
		}

		service.init(dc)

		if len(service.unitsByName) == 0 {
			logging.Errorf("Destinations are empty")
		}

	} else if destinationsSource != "" {
		if strings.HasPrefix(destinationsSource, "http://") || strings.HasPrefix(destinationsSource, "https://") {
			appconfig.Instance.AuthorizationService.DestinationsForceReload = resources.Watch(serviceName, destinationsSource, resources.LoadFromHTTP, service.updateDestinations, time.Duration(reloadSec)*time.Second)
		} else if strings.Contains(destinationsSource, "file://") || strings.HasPrefix(destinationsSource, "/") {
			appconfig.Instance.AuthorizationService.DestinationsForceReload = resources.Watch(serviceName, strings.Replace(destinationsSource, "file://", "", 1), resources.LoadFromFile, service.updateDestinations, time.Duration(reloadSec)*time.Second)
		} else if strings.HasPrefix(destinationsSource, "{") && strings.HasSuffix(destinationsSource, "}") {
			service.updateDestinations([]byte(destinationsSource))
		} else {
			return nil, errors.New("Unknown destination source: " + destinationsSource)
		}
	} else {
		logging.Errorf("Destinations aren't configured")
	}

	return service, nil
}

func (s *Service) GetConsumers(tokenID string) (consumers []events.Consumer) {
	s.RLock()
	defer s.RUnlock()
	for _, c := range s.consumersByTokenID[tokenID] {
		consumers = append(consumers, c)
	}
	return
}

func (s *Service) GetStorageByID(id string) (storages.StorageProxy, bool) {
	s.RLock()
	defer s.RUnlock()

	unit, ok := s.unitsByName[id]
	if !ok {
		return nil, false
	}

	return unit.storage, true
}

func (s *Service) GetStorages(tokenID string) (storages []storages.StorageProxy) {
	s.RLock()
	defer s.RUnlock()
	for _, s := range s.storagesByTokenID[tokenID] {
		storages = append(storages, s)
	}
	return
}

func (s *Service) GetDestinationIDs(tokenID string) map[string]bool {
	ids := map[string]bool{}
	s.RLock()
	defer s.RUnlock()
	for id := range s.destinationsIDByTokenID[tokenID] {
		ids[id] = true
	}
	return ids
}

func (s *Service) updateDestinations(payload []byte) {
	dc, err := parseFromBytes(payload)
	if err != nil {
		logging.Error(marshallingErrorMsg, err)
		return
	}

	s.init(dc)

	if len(s.unitsByName) == 0 {
		logging.Errorf("Destinations are empty")
	}
}

//1. close and remove all destinations which don't exist in new config
//2. recreate/create changed/new destinations
func (s *Service) init(dc map[string]storages.DestinationConfig) {
	StatusInstance.Reloading = true

	//close and remove non-existent (in new config)
	toDelete := map[string]*Unit{}
	for name, unit := range s.unitsByName {
		_, ok := dc[name]
		if !ok {
			toDelete[name] = unit
		}
	}
	if len(toDelete) > 0 {
		s.Lock()
		for name, unit := range toDelete {
			s.remove(name, unit)
		}
		s.Unlock()
	}

	// create or recreate
	newConsumers := TokenizedConsumers{}
	newStorages := TokenizedStorages{}
	newIDs := TokenizedIDs{}
	for destinationName, d := range dc {
		//common case
		destinationConfig := d
		name := destinationName

		//map token -> id
		if len(destinationConfig.OnlyTokens) > 0 {
			destinationConfig.OnlyTokens = appconfig.Instance.AuthorizationService.GetAllIDsByToken(destinationConfig.OnlyTokens)
		} else {
			logging.Warnf("[%s] only_tokens aren't provided. All tokens will be stored.", name)
			destinationConfig.OnlyTokens = appconfig.Instance.AuthorizationService.GetAllTokenIDs()
		}

		hash := getHash(name, destinationConfig)
		unit, ok := s.unitsByName[name]
		if ok {
			if unit.hash == hash {
				//destination wasn't changed
				continue
			}
			//remove old (for recreation)
			s.Lock()
			s.remove(name, unit)
			s.Unlock()
		}

		if len(destinationConfig.OnlyTokens) == 0 {
			logging.Warnf("[%s] destination's authorization isn't ready. Will be created in next reloading cycle.", name)
			//authorization tokens weren't loaded => create this destination when authorization service will be reloaded
			//and call force reload on this service
			continue
		}

		//create new
		newStorageProxy, eventQueue, err := s.storageFactory.Create(name, destinationConfig)
		if err != nil {
			logging.Errorf("[%s] Error initializing destination of type %s: %v", name, destinationConfig.Type, err)
			continue
		}

		s.unitsByName[name] = &Unit{
			eventQueue: eventQueue,
			storage:    newStorageProxy,
			tokenIDs:   destinationConfig.OnlyTokens,
			hash:       hash,
		}

		//create:
		//  1 logger per token id
		//  1 queue per destination id
		//append:
		//  storage per token id
		//  consumers per client_secret and server_secret
		// If destination is staged, consumer must not be added as staged
		// destinations may be used only by dry-run functionality
		for _, tokenID := range destinationConfig.OnlyTokens {
			if destinationConfig.Staged {
				logging.Warnf("[%s] Skipping consumer creation for staged destination", name)
				continue
			}
			newIDs.Add(tokenID, name)
			if destinationConfig.Mode == storages.StreamMode {
				newConsumers.Add(tokenID, name, eventQueue)
			} else {
				//get or create new logger
				loggerUsage, ok := s.loggersUsageByTokenID[tokenID]
				if !ok {
					incomeLogger := s.loggerFactory.CreateIncomingLogger(tokenID)
					loggerUsage = &LoggerUsage{logger: incomeLogger, usage: 0}
					s.loggersUsageByTokenID[tokenID] = loggerUsage
				}

				if loggerUsage != nil {
					loggerUsage.usage += 1
					//2 destinations with only 1 logger can be under 1 tokenID
					newConsumers.Add(tokenID, tokenID, loggerUsage.logger)
				}

				//add storage only if batch mode
				newStorages.Add(tokenID, name, newStorageProxy)
			}
		}
	}

	s.Lock()
	s.consumersByTokenID.AddAll(newConsumers)
	s.storagesByTokenID.AddAll(newStorages)
	s.destinationsIDByTokenID.AddAll(newIDs)
	s.Unlock()

	StatusInstance.Reloading = false
}

//remove destination from all collections and close it
//method must be called with locks
func (s *Service) remove(name string, unit *Unit) {
	//remove from other collections: queue or logger(if needed) + storage
	for _, tokenID := range unit.tokenIDs {
		oldConsumers := s.consumersByTokenID[tokenID]
		if unit.eventQueue != nil {
			delete(oldConsumers, name)
		} else {
			//logger
			loggerUsage := s.loggersUsageByTokenID[tokenID]
			loggerUsage.usage -= 1
			if loggerUsage.usage == 0 {
				delete(oldConsumers, tokenID)
				delete(s.loggersUsageByTokenID, tokenID)
				loggerUsage.logger.Close()
			}
		}

		if len(oldConsumers) == 0 {
			delete(s.consumersByTokenID, tokenID)
		}

		//storage
		oldStorages, ok := s.storagesByTokenID[tokenID]
		if ok {
			delete(oldStorages, name)
			if len(oldStorages) == 0 {
				delete(s.storagesByTokenID, tokenID)
			}
		}

		//id
		ids, ok := s.destinationsIDByTokenID[tokenID]
		if ok {
			delete(ids, name)
			if len(ids) == 0 {
				delete(s.destinationsIDByTokenID, tokenID)
			}
		}
	}

	if err := unit.Close(); err != nil {
		logging.Errorf("[%s] Error closing destination unit: %v", name, err)
	}

	delete(s.unitsByName, name)
	logging.Infof("[%s] has been removed!", name)
}

func (s *Service) Close() (multiErr error) {
	for token, loggerUsage := range s.loggersUsageByTokenID {
		if err := loggerUsage.logger.Close(); err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("Error closing logger for token [%s]: %v", token, err))
		}
	}

	for name, unit := range s.unitsByName {
		if err := unit.Close(); err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("[%s] Error closing destination unit: %v", name, err))
		}
	}

	return
}