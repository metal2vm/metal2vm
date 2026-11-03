import {
  DocumentTitle,
  ListPageHeader,
  ResourceLink,
  Timestamp,
  useActiveNamespace,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import { Bullseye, EmptyState, EmptyStateBody, PageSection, Spinner } from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { useTranslation } from 'react-i18next';
import type { FC } from 'react';

import { P2VMigrationGVK, type P2VMigration } from '../models';

const ALL_NAMESPACES = '#ALL_NS#';

const MigrationsPage: FC = () => {
  const { t } = useTranslation('plugin__metal2vm-console-plugin');
  const [activeNamespace] = useActiveNamespace();
  const [migrations, loaded, loadError] = useK8sWatchResource<P2VMigration[]>({
    groupVersionKind: P2VMigrationGVK,
    isList: true,
    namespace: activeNamespace === ALL_NAMESPACES ? undefined : activeNamespace,
  }) as [P2VMigration[], boolean, unknown];
  const hasError = Boolean(loadError);

  return (
    <>
      <DocumentTitle>{t('Physical migrations')}</DocumentTitle>
      <ListPageHeader title={t('Physical migrations')} />
      <PageSection>
        {hasError && (
          <EmptyState titleText={t('Unable to load migrations')} headingLevel="h2">
            <EmptyStateBody>
              {loadError instanceof Error ? loadError.message : JSON.stringify(loadError)}
            </EmptyStateBody>
          </EmptyState>
        )}
        {!hasError && !loaded && (
          <Bullseye>
            <Spinner aria-label={t('Loading')} />
          </Bullseye>
        )}
        {!hasError && loaded && migrations.length === 0 && (
          <EmptyState titleText={t('No physical migrations yet')} headingLevel="h2">
            <EmptyStateBody>
              {t('Migrations of physical servers to virtual machines will appear here.')}
            </EmptyStateBody>
          </EmptyState>
        )}
        {!hasError && loaded && migrations.length > 0 && (
          <Table aria-label={t('Physical migrations')} variant="compact">
            <Thead>
              <Tr>
                <Th>{t('Name')}</Th>
                <Th>{t('Namespace')}</Th>
                <Th>{t('Source')}</Th>
                <Th>{t('Mode')}</Th>
                <Th>{t('Phase')}</Th>
                <Th>{t('Virtual machine')}</Th>
                <Th>{t('Created')}</Th>
              </Tr>
            </Thead>
            <Tbody>
              {migrations.map((m) => (
                <Tr key={m.metadata?.uid}>
                  <Td dataLabel={t('Name')}>
                    <ResourceLink
                      groupVersionKind={P2VMigrationGVK}
                      name={m.metadata?.name}
                      namespace={m.metadata?.namespace}
                    />
                  </Td>
                  <Td dataLabel={t('Namespace')}>
                    <ResourceLink
                      groupVersionKind={{ version: 'v1', kind: 'Namespace' }}
                      name={m.metadata?.namespace}
                    />
                  </Td>
                  <Td dataLabel={t('Source')}>{m.spec.sourceRef.name}</Td>
                  <Td dataLabel={t('Mode')}>{m.spec.mode ?? 'cold'}</Td>
                  <Td dataLabel={t('Phase')}>{m.status?.phase ?? 'Pending'}</Td>
                  <Td dataLabel={t('Virtual machine')}>
                    {m.status?.vmName ? (
                      <ResourceLink
                        groupVersionKind={{
                          group: 'kubevirt.io',
                          version: 'v1',
                          kind: 'VirtualMachine',
                        }}
                        name={m.status.vmName}
                        namespace={m.metadata?.namespace}
                      />
                    ) : (
                      '-'
                    )}
                  </Td>
                  <Td dataLabel={t('Created')}>
                    <Timestamp timestamp={m.metadata?.creationTimestamp} />
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </PageSection>
    </>
  );
};

export default MigrationsPage;
