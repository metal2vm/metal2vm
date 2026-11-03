import {
  Alert,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Label,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { useActiveNamespace } from '@openshift-console/dynamic-plugin-sdk';
import { useTranslation } from 'react-i18next';
import type { FC } from 'react';

import type { Condition, Inventory, MigrationPhase, OSConfig, P2VMigration } from '../models';
import { humanBytes } from '../utils/migration';
import './metal2vm.css';

export const NS = 'plugin__metal2vm-console-plugin';
export const ALL_NAMESPACES = '#ALL_NS#';

/** The active namespace, or "default" when "All projects" is selected. */
export const useNamespace = (): [string, boolean] => {
  const [ns] = useActiveNamespace() as [string, (ns: string) => void];
  return ns === ALL_NAMESPACES ? ['default', true] : [ns, false];
};

const phaseColor: Record<string, 'blue' | 'green' | 'orange' | 'red' | 'grey' | 'purple' | 'teal'> =
  {
    Pending: 'grey',
    Preflight: 'blue',
    Booting: 'blue',
    Connected: 'blue',
    Copying: 'blue',
    Converting: 'blue',
    Testing: 'purple',
    CuttingOver: 'orange',
    Completed: 'green',
    RollingBack: 'orange',
    RolledBack: 'grey',
    Failed: 'red',
  };

export const PhaseLabel: FC<{ phase?: MigrationPhase }> = ({ phase }) => (
  <Label color={phaseColor[phase ?? 'Pending'] ?? 'grey'}>{phase ?? 'Pending'}</Label>
);

export const ConditionsTable: FC<{ conditions?: Condition[] }> = ({ conditions }) => {
  const { t } = useTranslation(NS);
  if (!conditions?.length) return null;
  return (
    <Table aria-label={t('Conditions')} variant="compact">
      <Thead>
        <Tr>
          <Th>{t('Type')}</Th>
          <Th>{t('Status')}</Th>
          <Th>{t('Reason')}</Th>
          <Th>{t('Message')}</Th>
        </Tr>
      </Thead>
      <Tbody>
        {conditions.map((c) => (
          <Tr key={c.type}>
            <Td dataLabel={t('Type')}>{c.type}</Td>
            <Td dataLabel={t('Status')}>{c.status}</Td>
            <Td dataLabel={t('Reason')}>{c.reason}</Td>
            <Td dataLabel={t('Message')}>{c.message}</Td>
          </Tr>
        ))}
      </Tbody>
    </Table>
  );
};

/** Pre-flight results: blockers and warnings first. */
export const PreflightAlerts: FC<{ migration: P2VMigration }> = ({ migration }) => {
  const { t } = useTranslation(NS);
  const failed = (migration.status?.preflight ?? []).filter((p) => !p.passed);
  return (
    <>
      {failed.map((p) => (
        <Alert
          key={p.name}
          isInline
          variant={p.severity === 'Blocker' ? 'danger' : 'warning'}
          title={
            p.severity === 'Blocker'
              ? t('Blocker: {{name}}', { name: p.name })
              : t('Warning: {{name}}', { name: p.name })
          }
          className="metal2vm-spaced"
        >
          {p.message}
        </Alert>
      ))}
    </>
  );
};

export const InventorySummary: FC<{ inventory?: Inventory; osConfig?: OSConfig }> = ({
  inventory,
  osConfig,
}) => {
  const { t } = useTranslation(NS);
  if (!inventory && !osConfig) {
    return (
      <Alert isInline variant="info" title={t('No inventory yet')}>
        {t(
          'Boot the server from its metal2vm ISO (or let its BMC/PXE do it), or add OS credentials to run the pre-check.',
        )}
      </Alert>
    );
  }
  const cpu = inventory?.cpu;
  const os = osConfig?.os;
  return (
    <DescriptionList isHorizontal columnModifier={{ lg: '2Col' }}>
      <DescriptionListGroup>
        <DescriptionListTerm>{t('Operating system')}</DescriptionListTerm>
        <DescriptionListDescription>
          {os
            ? `${os.distribution ?? ''} ${os.version ?? ''} (${os.hostname ?? ''})`
            : t('Unknown until the pre-check or conversion')}
        </DescriptionListDescription>
      </DescriptionListGroup>
      <DescriptionListGroup>
        <DescriptionListTerm>{t('Firmware')}</DescriptionListTerm>
        <DescriptionListDescription>
          {inventory?.firmware?.type?.toUpperCase() ?? '-'}
          {inventory?.firmware?.secureBoot ? `, ${t('Secure Boot')}` : ''}
          {inventory?.firmware?.tpm ? ', TPM 2.0' : ''}
        </DescriptionListDescription>
      </DescriptionListGroup>
      <DescriptionListGroup>
        <DescriptionListTerm>{t('CPU')}</DescriptionListTerm>
        <DescriptionListDescription>
          {cpu
            ? `${cpu.model ?? ''} (${String(cpu.sockets ?? 1)} × ${String(cpu.coresPerSocket ?? 1)} × ${String(cpu.threadsPerCore ?? 1)})`
            : '-'}
        </DescriptionListDescription>
      </DescriptionListGroup>
      <DescriptionListGroup>
        <DescriptionListTerm>{t('Memory')}</DescriptionListTerm>
        <DescriptionListDescription>
          {inventory?.memoryMiB ? humanBytes(inventory.memoryMiB * 1024 * 1024) : '-'}
        </DescriptionListDescription>
      </DescriptionListGroup>
      <DescriptionListGroup>
        <DescriptionListTerm>{t('Disks')}</DescriptionListTerm>
        <DescriptionListDescription>
          {(inventory?.disks ?? [])
            .map((d) => `${humanBytes(d.sizeBytes)} ${d.transport ?? ''}`)
            .join(', ') || '-'}
        </DescriptionListDescription>
      </DescriptionListGroup>
      <DescriptionListGroup>
        <DescriptionListTerm>{t('Inventory source')}</DescriptionListTerm>
        <DescriptionListDescription>
          {inventory?.collectedBy === 'LiveAgent'
            ? t('Live environment')
            : inventory
              ? t('OS pre-check')
              : '-'}
        </DescriptionListDescription>
      </DescriptionListGroup>
    </DescriptionList>
  );
};

/** Warnings from the inventory and the OS pre-check. */
export const RiskAlerts: FC<{ inventory?: Inventory; osConfig?: OSConfig }> = ({
  inventory,
  osConfig,
}) => (
  <>
    {[...(inventory?.warnings ?? []), ...(osConfig?.warnings ?? [])].map((w) => (
      <Alert key={w} isInline variant="warning" title={w} className="metal2vm-spaced" />
    ))}
  </>
);
