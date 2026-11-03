import { DocumentTitle, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Bullseye,
  Button,
  Card,
  CardBody,
  CardTitle,
  ClipboardCopy,
  Flex,
  FlexItem,
  Label,
  PageSection,
  Spinner,
  Title,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router';
import type { FC } from 'react';
import type { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

import { P2VSourceGVK, SecretGVK, type P2VSource } from '../../models';
import { humanBytes } from '../../utils/migration';
import { ConditionsTable, InventorySummary, NS, RiskAlerts } from '../shared';
import '../metal2vm.css';

type BootSecret = K8sResourceCommon & { data?: Record<string, string> };

const decode = (b64?: string) => {
  try {
    return b64 ? atob(b64) : '';
  } catch {
    return '';
  }
};

const SourceDetailsPage: FC = () => {
  const { t } = useTranslation(NS);
  const { ns = 'default', name = '' } = useParams();
  const navigate = useNavigate();
  const [src, loaded, err] = useK8sWatchResource<P2VSource>({
    groupVersionKind: P2VSourceGVK,
    namespace: ns,
    name,
  }) as [P2VSource, boolean, unknown];
  const [boot] = useK8sWatchResource<BootSecret>({
    groupVersionKind: SecretGVK,
    namespace: ns,
    name: `${name}-boot`,
  }) as [BootSecret | undefined, boolean, unknown];

  if (err) {
    return (
      <PageSection>
        <Alert isInline variant="danger" title={t('Unable to load the server')}>
          {err instanceof Error ? err.message : JSON.stringify(err)}
        </Alert>
      </PageSection>
    );
  }
  if (!loaded) {
    return (
      <Bullseye>
        <Spinner aria-label={t('Loading')} />
      </Bullseye>
    );
  }
  const inv = src.status?.inventory;
  const osc = src.status?.osConfig;
  const isoURL = decode(boot?.data?.isoURL);
  const ipxeURL = decode(boot?.data?.ipxeHTTPURL) || decode(boot?.data?.ipxeURL);
  const nicIPs = new Map((osc?.nics ?? []).map((n) => [n.mac, n]));

  return (
    <>
      <DocumentTitle>{name}</DocumentTitle>
      <PageSection>
        <Flex justifyContent={{ default: 'justifyContentSpaceBetween' }}>
          <FlexItem>
            <Title headingLevel="h1">
              {name}{' '}
              {src.status?.agent?.connected && (
                <Label color="green">{t('Live agent connected')}</Label>
              )}
            </Title>
          </FlexItem>
          <FlexItem>
            <Button
              variant="primary"
              isDisabled={!inv}
              onClick={() => void navigate(`/p2v/ns/${ns}/migrations/~new?source=${name}`)}
            >
              {t('Migrate this server')}
            </Button>
          </FlexItem>
        </Flex>

        <Card className="metal2vm-section">
          <CardTitle>{t('Overview')}</CardTitle>
          <CardBody>
            <RiskAlerts inventory={inv} osConfig={osc} />
            <InventorySummary inventory={inv} osConfig={osc} />
          </CardBody>
        </Card>

        <Card className="metal2vm-section">
          <CardTitle>{t('Boot media')}</CardTitle>
          <CardBody>
            {isoURL ? (
              <>
                <p className="metal2vm-spaced">
                  {t(
                    'Boot the server from this ISO (USB stick or BMC virtual media). It stops working at {{time}}.',
                    {
                      time: src.status?.bootMedia?.expiresAt ?? '-',
                    },
                  )}
                </p>
                <Button component="a" href={isoURL} variant="secondary" className="metal2vm-spaced">
                  {t('Download ISO')}
                </Button>
                {ipxeURL && (
                  <>
                    <p className="metal2vm-spaced">{t('iPXE script for network boot:')}</p>
                    <ClipboardCopy isReadOnly hoverTip={t('Copy')} clickTip={t('Copied')}>
                      {ipxeURL}
                    </ClipboardCopy>
                  </>
                )}
              </>
            ) : (
              <Alert
                isInline
                variant="info"
                title={t('Boot media is being prepared, or you lack access to Secrets here.')}
              />
            )}
          </CardBody>
        </Card>

        {(inv?.disks?.length ?? 0) > 0 && (
          <Card className="metal2vm-section">
            <CardTitle>{t('Disks')}</CardTitle>
            <CardBody>
              <Table aria-label={t('Disks')} variant="compact">
                <Thead>
                  <Tr>
                    <Th>{t('ID')}</Th>
                    <Th>{t('Size')}</Th>
                    <Th>{t('Model')}</Th>
                    <Th>{t('Attached via')}</Th>
                    <Th>{t('Notes')}</Th>
                  </Tr>
                </Thead>
                <Tbody>
                  {(inv?.disks ?? []).map((d) => (
                    <Tr key={d.id}>
                      <Td dataLabel={t('ID')}>{d.id}</Td>
                      <Td dataLabel={t('Size')}>{humanBytes(d.sizeBytes)}</Td>
                      <Td dataLabel={t('Model')}>{d.model}</Td>
                      <Td dataLabel={t('Attached via')}>{d.transport}</Td>
                      <Td dataLabel={t('Notes')}>
                        {d.multipath && <Label isCompact>{t('multipath')}</Label>}{' '}
                        {d.encrypted && (
                          <Label isCompact color="orange">
                            {t('encrypted')}
                          </Label>
                        )}
                      </Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            </CardBody>
          </Card>
        )}

        {(inv?.nics?.length ?? osc?.nics?.length ?? 0) > 0 && (
          <Card className="metal2vm-section">
            <CardTitle>{t('Network interfaces')}</CardTitle>
            <CardBody>
              <Table aria-label={t('Network interfaces')} variant="compact">
                <Thead>
                  <Tr>
                    <Th>{t('MAC')}</Th>
                    <Th>{t('Name in the OS')}</Th>
                    <Th>{t('IP addresses')}</Th>
                    <Th>{t('Configuration')}</Th>
                  </Tr>
                </Thead>
                <Tbody>
                  {(inv?.nics ?? osc?.nics ?? []).map((n) => {
                    const o = nicIPs.get(n.mac);
                    return (
                      <Tr key={n.mac}>
                        <Td dataLabel={t('MAC')}>{n.mac}</Td>
                        <Td dataLabel={t('Name in the OS')}>{o?.name ?? '-'}</Td>
                        <Td dataLabel={t('IP addresses')}>
                          {(o?.ipAddresses ?? []).join(', ') || '-'}
                        </Td>
                        <Td dataLabel={t('Configuration')}>
                          {o ? (o.dhcp ? 'DHCP' : t('static')) : t('run the OS pre-check')}
                        </Td>
                      </Tr>
                    );
                  })}
                </Tbody>
              </Table>
            </CardBody>
          </Card>
        )}

        <Card className="metal2vm-section">
          <CardTitle>{t('Conditions')}</CardTitle>
          <CardBody>
            <ConditionsTable conditions={src.status?.conditions} />
          </CardBody>
        </Card>
      </PageSection>
    </>
  );
};

export default SourceDetailsPage;
