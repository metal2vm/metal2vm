import {
  DocumentTitle,
  ListPageCreateButton,
  ListPageHeader,
  NamespaceBar,
  ResourceLink,
  Timestamp,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import { Bullseye, EmptyState, EmptyStateBody, PageSection, Spinner } from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router';
import type { FC } from 'react';

import { P2VMigrationGVK, VirtualMachineGVK, type P2VMigration } from '../../models';
import { copyPercent } from '../../utils/migration';
import { NS, PhaseLabel, useNamespace } from '../shared';

const MigrationsPage: FC = () => {
  const { t } = useTranslation(NS);
  const navigate = useNavigate();
  const [ns, all] = useNamespace();
  const [migrations, loaded, loadError] = useK8sWatchResource<P2VMigration[]>({
    groupVersionKind: P2VMigrationGVK,
    isList: true,
    namespace: all ? undefined : ns,
  }) as [P2VMigration[], boolean, unknown];
  const hasError = Boolean(loadError);

  return (
    <>
      <DocumentTitle>{t('Physical migrations')}</DocumentTitle>
      <NamespaceBar />
      <ListPageHeader title={t('Physical migrations')}>
        <ListPageCreateButton onClick={() => void navigate(`/p2v/ns/${ns}/migrations/~new`)}>
          {t('Migrate a server')}
        </ListPageCreateButton>
      </ListPageHeader>
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
                <Th modifier="nowrap">{t('Name')}</Th>
                {all && <Th>{t('Namespace')}</Th>}
                <Th modifier="nowrap">{t('Source')}</Th>
                <Th>{t('Phase')}</Th>
                <Th>{t('Progress')}</Th>
                <Th>{t('Virtual machine')}</Th>
                <Th>{t('Created')}</Th>
              </Tr>
            </Thead>
            <Tbody>
              {migrations.map((m) => {
                const namespace = m.metadata?.namespace ?? ns;
                return (
                  <Tr key={m.metadata?.uid}>
                    <Td dataLabel={t('Name')} modifier="nowrap">
                      <Link to={`/p2v/ns/${namespace}/migrations/${m.metadata?.name ?? ''}`}>
                        {m.metadata?.name}
                      </Link>
                    </Td>
                    {all && <Td dataLabel={t('Namespace')}>{namespace}</Td>}
                    <Td dataLabel={t('Source')} modifier="nowrap">
                      <Link to={`/p2v/ns/${namespace}/sources/${m.spec.sourceRef.name}`}>
                        {m.spec.sourceRef.name}
                      </Link>
                    </Td>
                    <Td dataLabel={t('Phase')}>
                      <PhaseLabel phase={m.status?.phase} />
                    </Td>
                    <Td dataLabel={t('Progress')}>
                      {m.status?.phase === 'Copying'
                        ? `${String(copyPercent(m))}%`
                        : m.status?.message}
                    </Td>
                    <Td dataLabel={t('Virtual machine')} modifier="nowrap">
                      {m.status?.vmName ? (
                        <ResourceLink
                          groupVersionKind={VirtualMachineGVK}
                          name={m.status.vmName}
                          namespace={namespace}
                        />
                      ) : (
                        '-'
                      )}
                    </Td>
                    <Td dataLabel={t('Created')} modifier="nowrap">
                      <Timestamp timestamp={m.metadata?.creationTimestamp} />
                    </Td>
                  </Tr>
                );
              })}
            </Tbody>
          </Table>
        )}
      </PageSection>
    </>
  );
};

export default MigrationsPage;
