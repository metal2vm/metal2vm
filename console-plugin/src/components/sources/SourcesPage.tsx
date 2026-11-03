import {
  DocumentTitle,
  ListPageCreateButton,
  ListPageHeader,
  NamespaceBar,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import {
  Bullseye,
  Button,
  EmptyState,
  EmptyStateBody,
  Label,
  PageSection,
  Spinner,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router';
import type { FC } from 'react';

import { P2VSourceGVK, type P2VSource } from '../../models';
import { NS, useNamespace } from '../shared';
import '../metal2vm.css';

const accessLabels = (s: P2VSource) =>
  [s.spec.bmc && 'BMC', s.spec.pxe && 'PXE', s.spec.osAccess?.type.toUpperCase()].filter(
    Boolean,
  ) as string[];

const SourcesPage: FC = () => {
  const { t } = useTranslation(NS);
  const navigate = useNavigate();
  const [ns, all] = useNamespace();
  const [sources, loaded, loadError] = useK8sWatchResource<P2VSource[]>({
    groupVersionKind: P2VSourceGVK,
    isList: true,
    namespace: all ? undefined : ns,
  }) as [P2VSource[], boolean, unknown];

  return (
    <>
      <DocumentTitle>{t('Physical servers')}</DocumentTitle>
      <NamespaceBar />
      <ListPageHeader title={t('Physical servers')}>
        <ListPageCreateButton onClick={() => void navigate(`/p2v/ns/${ns}/sources/~new`)}>
          {t('Add physical server')}
        </ListPageCreateButton>
      </ListPageHeader>
      <PageSection>
        {Boolean(loadError) && (
          <EmptyState titleText={t('Unable to load physical servers')} headingLevel="h2">
            <EmptyStateBody>
              {loadError instanceof Error ? loadError.message : JSON.stringify(loadError)}
            </EmptyStateBody>
          </EmptyState>
        )}
        {!loadError && !loaded && (
          <Bullseye>
            <Spinner aria-label={t('Loading')} />
          </Bullseye>
        )}
        {!loadError && loaded && sources.length === 0 && (
          <EmptyState titleText={t('No physical servers yet')} headingLevel="h2">
            <EmptyStateBody>
              {t(
                'Add a physical server with its BMC, PXE MAC address or OS credentials to start migrating it.',
              )}
            </EmptyStateBody>
          </EmptyState>
        )}
        {!loadError && loaded && sources.length > 0 && (
          <Table aria-label={t('Physical servers')} variant="compact">
            <Thead>
              <Tr>
                <Th>{t('Name')}</Th>
                {all && <Th>{t('Namespace')}</Th>}
                <Th>{t('Access')}</Th>
                <Th>{t('Hardware')}</Th>
                <Th>{t('Operating system')}</Th>
                <Th modifier="wrap">{t('Live agent')}</Th>
                <Th screenReaderText={t('Actions')} />
              </Tr>
            </Thead>
            <Tbody>
              {sources.map((s) => {
                const name = s.metadata?.name ?? '';
                const namespace = s.metadata?.namespace ?? ns;
                const inv = s.status?.inventory;
                const os = s.status?.osConfig?.os;
                return (
                  <Tr key={s.metadata?.uid}>
                    <Td dataLabel={t('Name')} modifier="nowrap">
                      <Link to={`/p2v/ns/${namespace}/sources/${name}`}>{name}</Link>
                    </Td>
                    {all && <Td dataLabel={t('Namespace')}>{namespace}</Td>}
                    <Td dataLabel={t('Access')}>
                      {accessLabels(s).map((a) => (
                        <Label key={a} isCompact className="pf-v6-u-mr-xs">
                          {a}
                        </Label>
                      ))}
                      {accessLabels(s).length === 0 && t('Manual (USB/ISO)')}
                    </Td>
                    <Td dataLabel={t('Hardware')}>
                      {s.status?.bmc?.model ??
                        (inv
                          ? `${String(inv.cpu?.sockets ?? 1)}×${String(inv.cpu?.coresPerSocket ?? 1)} CPU, ${String(inv.disks?.length ?? 0)} disks`
                          : '-')}
                    </Td>
                    <Td dataLabel={t('Operating system')}>
                      {os ? `${os.distribution ?? ''} ${os.version ?? ''}` : '-'}
                    </Td>
                    <Td dataLabel={t('Live agent')}>
                      {s.status?.agent?.connected ? (
                        <Label color="green">{t('Connected')}</Label>
                      ) : (
                        '-'
                      )}
                    </Td>
                    <Td isActionCell>
                      <Button
                        variant="secondary"
                        size="sm"
                        isDisabled={!inv}
                        onClick={() =>
                          void navigate(`/p2v/ns/${namespace}/migrations/~new?source=${name}`)
                        }
                      >
                        {t('Migrate')}
                      </Button>
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

export default SourcesPage;
