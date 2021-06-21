// @Libs
import React, { useCallback, useEffect, useMemo, useState } from 'react';
// @Styles
import styles from './OnboardingTourAddDestination.module.less';
// @Commons
import { createFreeDatabase } from '@./lib/commons/createFreeDatabase';
// @Components
import { EmptyListView } from '@./ui/components/EmptyList/EmptyListView';
import { DropDownList } from '@./ui/components/DropDownList/DropDownList';
import { DestinationEditor } from 'ui/pages/DestinationsPage/partials/DestinationEditor/DestinationEditor';
import {
  destinationsReferenceList,
  destinationsReferenceMap,
  DestinationStrictType
} from '@./ui/pages/DestinationsPage/commons';
// @Hooks
import useLoader from '@./hooks/useLoader';
// @Utils
import ApiKeyHelper from '@./lib/services/ApiKeyHelper';
import ApplicationServices from '@./lib/services/ApplicationServices';

type ExtractDatabaseOrWebhook<T> = T extends {readonly type: 'database'}
  ? T
  : T extends {readonly id: 'webhook'}
    ? T
    : never;

const destinationsToOffer = destinationsReferenceList.filter(
  (dest): dest is ExtractDatabaseOrWebhook<DestinationStrictType> => {
    return dest.type === 'database' || dest.id === 'webhook';
  }
)

type NamesOfDestinationsToOffer = (typeof destinationsToOffer)[number]['id'];

type Lifecycle =
  | 'loading'
  | 'setup_choice'
  | NamesOfDestinationsToOffer;

type Props = {
   handleGoNext: () => void;
 }

const services = ApplicationServices.get();

export const OnboardingTourAddDestination: React.FC<Props> = function({
  handleGoNext
}) {
  const [lifecycle, setLifecycle] = useState<Lifecycle>('loading');

  const [sourcesError, sources, updateSources,, isLoadingUserSources] = useLoader(
    async() => await services.storageService.get('sources', services.activeProject.id)
  );
  const [, destinations,, updateDestinations, isLoadingUserDestinations ] = useLoader(
    async() => await services.storageService.get('destinations', services.activeProject.id)
  );

  const userSources = useMemo(() => sources?.sources ?? [], [sources])
  const userDestinations = useMemo(() =>  destinations?.destinations ?? [], [destinations])

  const handleCancelDestinationSetup = useCallback<() => void>(() => {
    setLifecycle('setup_choice');
  }, []);

  const onAfterCustomDestinationCreated = useCallback<() => Promise<void>>(async() => {
    const helper = new ApiKeyHelper(services);
    await helper.init();

    // if user created a destination at this step, it is his first destination
    const destination = helper.destinations[0];
    if (!destination) {
      services.analyticsService.track(
        'onboarding_destination_error_custom',
        { error: 'onAfterCustomDestinationCreated failed to extract a destination' }
      );
      handleGoNext();
      return;
    }

    // track successful destination creation
    services.analyticsService.track(`onboarding_destination_created_${destination._type}`);

    // user might have multiple keys - we are using the first one
    let key = helper.keys[0];
    if (!key) key = await helper.createNewAPIKey();
    await helper.linkKeyToDestination(key, destination);

    handleGoNext();
  }, [handleGoNext])

  const handleCreateFreeDatabase = useCallback<() => Promise<void>>(async() => {
    try {
      await createFreeDatabase();
    } catch (error) {
      services.analyticsService.track('onboarding_destination_error_free', { error });
    }
    services.analyticsService.track('onboarding_destination_created_free');
    handleGoNext();
  }, [handleGoNext])

  const render = useMemo<React.ReactElement>(() => {
    switch (lifecycle) {

    case 'loading':
      return null;

    case 'setup_choice':
      const list = <DropDownList
        hideFilter
        list={destinationsToOffer.map((dst) => ({
          title: dst.displayName,
          id: dst.id,
          icon: dst.ui.icon,
          handleClick: () => setLifecycle(dst.id)
        }))}
      />
      return (
        <>
          <p className={styles.paragraph}>
            {`Looks like you don't have destinations set up. Let's create one.`}
          </p>
          <div className={styles.addDestinationButtonContainer}>
            <EmptyListView
              title=""
              list={list}
              unit="destination"
              centered={false}
              dropdownOverlayPlacement="bottomCenter"
              handleCreateFreeDatabase={handleCreateFreeDatabase}
              // showFreeDatabaseSeparateButton={false}
            />
          </div>
        </>
      );

    default:
      return (
        <div className={styles.destinationEditorContainer}>
          <DestinationEditor
            destinations={userDestinations}
            setBreadcrumbs={() => {}}
            updateDestinations={updateDestinations}
            editorMode="add"
            sources={userSources}
            sourcesError={sourcesError}
            updateSources={updateSources}
            paramsByProps={{
              type: destinationsReferenceMap[lifecycle]['id'],
              id: '',
              tabName: 'tab'
            }}
            disableForceUpdateOnSave
            onAfterSaveSucceded={onAfterCustomDestinationCreated}
            onCancel={handleCancelDestinationSetup}
          />
        </div>
      );
    }
  }, [
    lifecycle,
    userDestinations,
    userSources,
    sourcesError,
    updateDestinations,
    updateSources,
    handleCancelDestinationSetup,
    onAfterCustomDestinationCreated,
    handleCreateFreeDatabase
  ])

  useEffect(() => {
    if (!isLoadingUserDestinations && !isLoadingUserSources) setLifecycle('setup_choice')
  }, [isLoadingUserDestinations, isLoadingUserSources])

  return (
    <div className={styles.mainContainer}>
      <h1 className={styles.header}>
        {'🔗 Destinations Setup'}
      </h1>
      {render}
    </div>
  );
}