import { DocumentTitle, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Bullseye,
  Checkbox,
  CodeBlock,
  CodeBlockCode,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  ExpandableSection,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  NumberInput,
  PageSection,
  Radio,
  Spinner,
  TextInput,
  Title,
  Wizard,
  WizardStep,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { useMemo, useState, type FC } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams, useSearchParams } from 'react-router';

import {
  NADGVK,
  P2VMigrationGVK,
  P2VMigrationModel,
  P2VSourceGVK,
  StorageClassGVK,
  type NetworkAttachmentDefinition,
  type P2VMigration,
  type P2VSource,
  type StorageClass,
} from '../../models';
import {
  buildMigration,
  humanBytes,
  initialState,
  validate,
  type DiskChoice,
  type NICChoice,
  type WizardState,
} from '../../utils/migration';
import { createResource } from '../../utils/api';
import { InventorySummary, NS, RiskAlerts } from '../shared';
import '../metal2vm.css';

const DEFAULT_SC = 'storageclass.kubernetes.io/is-default-class';

const MigrationWizard: FC = () => {
  const { t } = useTranslation(NS);
  const { ns = 'default' } = useParams();
  const [params] = useSearchParams();
  const navigate = useNavigate();

  const [sources, sourcesLoaded] = useK8sWatchResource<P2VSource[]>({
    groupVersionKind: P2VSourceGVK,
    isList: true,
    namespace: ns,
  }) as [P2VSource[], boolean, unknown];
  const [scs] = useK8sWatchResource<StorageClass[]>({
    groupVersionKind: StorageClassGVK,
    isList: true,
  }) as [StorageClass[], boolean, unknown];
  const [nads] = useK8sWatchResource<NetworkAttachmentDefinition[]>({
    groupVersionKind: NADGVK,
    isList: true,
    namespace: ns,
  }) as [NetworkAttachmentDefinition[], boolean, unknown];
  const [migrations] = useK8sWatchResource<P2VMigration[]>({
    groupVersionKind: P2VMigrationGVK,
    isList: true,
    namespace: ns,
  }) as [P2VMigration[], boolean, unknown];
  const takenNames = migrations.map((m) => m.metadata?.name ?? '');

  const [sourceName, setSourceName] = useState(params.get('source') ?? '');
  const src = useMemo(
    () => sources.find((s) => s.metadata?.name === sourceName),
    [sources, sourceName],
  );
  // Wizard state = defaults from the chosen server's inventory + the admin's
  // edits, kept per server so switching servers does not lose choices.
  const [edits, setEdits] = useState<Partial<Record<string, WizardState>>>({});
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const base = src?.status?.inventory ? initialState(ns, src, takenNames) : null;
  const state = edits[sourceName] ?? base;

  const update = (fn: (s: WizardState) => WizardState) => {
    setEdits((e) => {
      const cur = e[sourceName] ?? base;
      return cur ? { ...e, [sourceName]: fn(cur) } : e;
    });
  };
  const set = <K extends keyof WizardState>(k: K, v: WizardState[K]) => {
    update((s) => ({ ...s, [k]: v }));
  };
  const setDisk = (i: number, d: Partial<DiskChoice>) => {
    update((s) => ({ ...s, disks: s.disks.map((x, j) => (j === i ? { ...x, ...d } : x)) }));
  };
  const setNIC = (i: number, n: Partial<NICChoice>) => {
    update((s) => ({ ...s, nics: s.nics.map((x, j) => (j === i ? { ...x, ...n } : x)) }));
  };

  if (!sourcesLoaded) {
    return (
      <Bullseye>
        <Spinner aria-label={t('Loading')} />
      </Bullseye>
    );
  }
  const inventoried = sources.filter((s) => s.status?.inventory);
  const errors = state ? validate(state) : [t('Choose a server.')];
  if (state && takenNames.includes(state.name)) {
    errors.push(t('A migration named {{name}} already exists.', { name: state.name }));
  }
  const defaultSC = scs.find((s) => s.metadata?.annotations?.[DEFAULT_SC] === 'true')?.metadata
    ?.name;
  const osNIC = (mac: string) =>
    src?.status?.osConfig?.nics?.find((n) => n.mac.toLowerCase() === mac.toLowerCase());

  const create = async () => {
    if (!state) return;
    setBusy(true);
    setError('');
    try {
      await createResource(P2VMigrationModel, buildMigration(state));
      void navigate(`/p2v/ns/${ns}/migrations/${state.name}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  return (
    <>
      <DocumentTitle>{t('Migrate a physical server')}</DocumentTitle>
      <PageSection>
        <Title headingLevel="h1" className="metal2vm-spaced">
          {t('Migrate a physical server')}
        </Title>
        <Wizard onClose={() => void navigate(-1)} height={640}>
          <WizardStep name={t('Server')} id="server" footer={{ isNextDisabled: !state }}>
            <Form>
              <FormGroup label={t('Physical server')} isRequired fieldId="source">
                <FormSelect
                  id="source"
                  value={sourceName}
                  onChange={(_e, v) => {
                    setSourceName(v);
                  }}
                >
                  <FormSelectOption value="" label={t('Select a server')} isPlaceholder />
                  {inventoried.map((s) => (
                    <FormSelectOption
                      key={s.metadata?.uid}
                      value={s.metadata?.name}
                      label={s.metadata?.name ?? ''}
                    />
                  ))}
                </FormSelect>
              </FormGroup>
              {inventoried.length === 0 && (
                <Alert
                  isInline
                  variant="info"
                  title={t('No server with an inventory in this project')}
                >
                  {t(
                    'Add a physical server and boot it once from its ISO, or add OS credentials for the pre-check.',
                  )}
                </Alert>
              )}
              {src && (
                <>
                  <RiskAlerts inventory={src.status?.inventory} osConfig={src.status?.osConfig} />
                  <InventorySummary
                    inventory={src.status?.inventory}
                    osConfig={src.status?.osConfig}
                  />
                </>
              )}
              {state && src && (
                <FormGroup
                  role="radiogroup"
                  label={t('How the server boots the migration environment')}
                  fieldId="boot"
                >
                  <Radio
                    id="boot-redfish"
                    name="boot"
                    label={t('Through its BMC (automatic)')}
                    isDisabled={!src.spec.bmc}
                    isChecked={state.bootMethod === 'redfish'}
                    onChange={() => {
                      set('bootMethod', 'redfish');
                    }}
                  />
                  <Radio
                    id="boot-pxe"
                    name="boot"
                    label={t('Network boot (PXE)')}
                    isDisabled={!src.spec.pxe}
                    isChecked={state.bootMethod === 'pxe'}
                    onChange={() => {
                      set('bootMethod', 'pxe');
                    }}
                  />
                  <Radio
                    id="boot-manual"
                    name="boot"
                    label={t('I boot it from its ISO myself (USB stick or virtual media)')}
                    isChecked={state.bootMethod === 'manual'}
                    onChange={() => {
                      set('bootMethod', 'manual');
                    }}
                  />
                </FormGroup>
              )}
            </Form>
          </WizardStep>

          <WizardStep name={t('Disks')} id="disks" isDisabled={!state}>
            {state && (
              <Table aria-label={t('Disks')} variant="compact">
                <Thead>
                  <Tr>
                    <Th>{t('Disk')}</Th>
                    <Th>{t('Size')}</Th>
                    <Th>{t('Action')}</Th>
                    <Th>{t('Storage class / LUN')}</Th>
                  </Tr>
                </Thead>
                <Tbody>
                  {state.disks.map((d, i) => {
                    const inv = src?.status?.inventory?.disks?.find((x) => x.id === d.id);
                    return (
                      <Tr key={d.id}>
                        <Td dataLabel={t('Disk')}>
                          {inv?.model ?? d.id}
                          <div className="pf-v6-u-font-size-sm pf-v6-u-color-200">
                            {d.id} · {inv?.transport} {inv?.encrypted ? t('(encrypted)') : ''}
                          </div>
                        </Td>
                        <Td dataLabel={t('Size')}>{humanBytes(inv?.sizeBytes)}</Td>
                        <Td dataLabel={t('Action')}>
                          <FormSelect
                            aria-label={t('Action')}
                            value={d.action}
                            onChange={(_e, v) => {
                              setDisk(i, { action: v as DiskChoice['action'] });
                            }}
                          >
                            <FormSelectOption value="copy" label={t('Copy')} />
                            <FormSelectOption value="attach" label={t('Re-attach SAN LUN')} />
                            <FormSelectOption value="skip" label={t('Skip')} />
                          </FormSelect>
                        </Td>
                        <Td dataLabel={t('Storage class / LUN')}>
                          {d.action === 'copy' && (
                            <FormSelect
                              aria-label={t('Storage class')}
                              value={d.storageClassName}
                              onChange={(_e, v) => {
                                setDisk(i, { storageClassName: v });
                              }}
                            >
                              <FormSelectOption
                                value=""
                                label={t('Default ({{sc}})', { sc: defaultSC ?? '-' })}
                              />
                              {scs.map((sc) => (
                                <FormSelectOption
                                  key={sc.metadata?.name}
                                  value={sc.metadata?.name}
                                  label={sc.metadata?.name ?? ''}
                                />
                              ))}
                            </FormSelect>
                          )}
                          {d.action === 'attach' && (
                            <>
                              <TextInput
                                aria-label={t('iSCSI portal')}
                                placeholder={t('iSCSI portal, e.g. 10.0.0.5:3260')}
                                value={d.iscsi?.targetPortal ?? ''}
                                onChange={(_e, v) => {
                                  setDisk(i, {
                                    iscsi: { iqn: '', lun: 0, ...d.iscsi, targetPortal: v },
                                  });
                                }}
                              />
                              <TextInput
                                aria-label={t('IQN')}
                                placeholder={t('Target IQN')}
                                value={d.iscsi?.iqn ?? ''}
                                onChange={(_e, v) => {
                                  setDisk(i, {
                                    iscsi: { targetPortal: '', lun: 0, ...d.iscsi, iqn: v },
                                  });
                                }}
                              />
                              <NumberInput
                                aria-label={t('LUN')}
                                value={d.iscsi?.lun ?? 0}
                                min={0}
                                onMinus={() => {
                                  setDisk(i, {
                                    iscsi: {
                                      targetPortal: '',
                                      iqn: '',
                                      ...d.iscsi,
                                      lun: Math.max(0, (d.iscsi?.lun ?? 0) - 1),
                                    },
                                  });
                                }}
                                onPlus={() => {
                                  setDisk(i, {
                                    iscsi: {
                                      targetPortal: '',
                                      iqn: '',
                                      ...d.iscsi,
                                      lun: (d.iscsi?.lun ?? 0) + 1,
                                    },
                                  });
                                }}
                                onChange={(e) => {
                                  setDisk(i, {
                                    iscsi: {
                                      targetPortal: '',
                                      iqn: '',
                                      ...d.iscsi,
                                      lun: Number((e.target as HTMLInputElement).value) || 0,
                                    },
                                  });
                                }}
                              />
                            </>
                          )}
                        </Td>
                      </Tr>
                    );
                  })}
                </Tbody>
              </Table>
            )}
          </WizardStep>

          <WizardStep name={t('Network')} id="network" isDisabled={!state}>
            {state && (
              <>
                <Alert
                  isInline
                  variant="info"
                  className="metal2vm-spaced"
                  title={t('Keeping IP addresses')}
                >
                  {t(
                    'To keep a static IP, map the NIC to a bridged or localnet network for the same VLAN, without IP management (IPAM). On the pod network the VM gets a new address by DHCP.',
                  )}
                </Alert>
                <Table aria-label={t('Network interfaces')} variant="compact">
                  <Thead>
                    <Tr>
                      <Th>{t('Interface')}</Th>
                      <Th>{t('Network')}</Th>
                      <Th modifier="nowrap">{t('Keep MAC')}</Th>
                      <Th modifier="nowrap">{t('Keep IP')}</Th>
                    </Tr>
                  </Thead>
                  <Tbody>
                    {state.nics.map((n, i) => {
                      const o = osNIC(n.mac);
                      const bridged = n.network !== 'pod' && n.network !== 'none';
                      return (
                        <Tr key={n.mac}>
                          <Td dataLabel={t('Interface')}>
                            {o?.name ?? n.mac}
                            <div className="pf-v6-u-font-size-sm pf-v6-u-color-200">
                              {n.mac}{' '}
                              {o
                                ? `· ${(o.ipAddresses ?? []).join(', ')} ${o.dhcp ? '(DHCP)' : t('(static)')}`
                                : ''}
                            </div>
                          </Td>
                          <Td dataLabel={t('Network')}>
                            <FormSelect
                              aria-label={t('Network')}
                              value={n.network}
                              onChange={(_e, v) => {
                                setNIC(i, {
                                  network: v,
                                  ...(v === 'pod' || v === 'none'
                                    ? { keepIP: false, keepMAC: false }
                                    : {}),
                                });
                              }}
                            >
                              <FormSelectOption value="none" label={t('Not connected')} />
                              <FormSelectOption value="pod" label={t('Pod network (new IP)')} />
                              {nads.map((nad) => (
                                <FormSelectOption
                                  key={nad.metadata?.uid}
                                  value={nad.metadata?.name}
                                  label={nad.metadata?.name ?? ''}
                                />
                              ))}
                            </FormSelect>
                          </Td>
                          <Td dataLabel={t('Keep MAC')}>
                            <Checkbox
                              id={`keepmac-${String(i)}`}
                              aria-label={t('Keep MAC')}
                              isDisabled={!bridged}
                              isChecked={n.keepMAC || n.network === 'pod'}
                              onChange={(_e, v) => {
                                setNIC(i, { keepMAC: v });
                              }}
                            />
                          </Td>
                          <Td dataLabel={t('Keep IP')}>
                            <Checkbox
                              id={`keepip-${String(i)}`}
                              aria-label={t('Keep IP')}
                              isDisabled={!bridged}
                              isChecked={n.keepIP}
                              onChange={(_e, v) => {
                                setNIC(i, { keepIP: v });
                              }}
                            />
                          </Td>
                        </Tr>
                      );
                    })}
                  </Tbody>
                </Table>
              </>
            )}
          </WizardStep>

          <WizardStep name={t('Virtual machine')} id="vm" isDisabled={!state}>
            {state && (
              <Form>
                <FormGroup label={t('Migration name')} isRequired fieldId="mig-name">
                  <TextInput
                    id="mig-name"
                    value={state.name}
                    onChange={(_e, v) => {
                      set('name', v);
                    }}
                  />
                </FormGroup>
                <FormGroup label={t('Virtual machine name')} fieldId="vm-name">
                  <TextInput
                    id="vm-name"
                    value={state.vmName}
                    onChange={(_e, v) => {
                      set('vmName', v);
                    }}
                  />
                </FormGroup>
                <FormGroup label={t("Memory (empty keeps the server's memory)")} fieldId="vm-mem">
                  <TextInput
                    id="vm-mem"
                    value={state.memory}
                    placeholder="16Gi"
                    onChange={(_e, v) => {
                      set('memory', v);
                    }}
                  />
                </FormGroup>
                <FormGroup
                  label={t("CPU cores (0 keeps the server's topology)")}
                  fieldId="vm-cores"
                >
                  <NumberInput
                    id="vm-cores"
                    value={state.cores}
                    min={0}
                    onMinus={() => {
                      set('cores', Math.max(0, state.cores - 1));
                    }}
                    onPlus={() => {
                      set('cores', state.cores + 1);
                    }}
                    onChange={(e) => {
                      set('cores', Number((e.target as HTMLInputElement).value) || 0);
                    }}
                  />
                </FormGroup>
                <Checkbox
                  id="test-boot"
                  label={t('Test-boot the VM without network before cutting over (recommended)')}
                  isChecked={state.testBoot}
                  onChange={(_e, v) => {
                    set('testBoot', v);
                  }}
                />
                <Checkbox
                  id="auto-shutdown"
                  label={t(
                    'Power off the physical server at cutover (recommended, avoids duplicate IPs)',
                  )}
                  isChecked={state.autoShutdownSource}
                  onChange={(_e, v) => {
                    set('autoShutdownSource', v);
                  }}
                />
              </Form>
            )}
          </WizardStep>

          <WizardStep
            name={t('Review')}
            id="review"
            isDisabled={!state}
            footer={{
              nextButtonText: t('Start migration'),
              isNextDisabled: errors.length > 0 || busy,
              onNext: () => void create(),
            }}
          >
            {errors.map((e) => (
              <Alert key={e} isInline variant="danger" title={e} className="metal2vm-spaced" />
            ))}
            {error && (
              <Alert
                isInline
                variant="danger"
                title={t('Could not create the migration')}
                className="metal2vm-spaced"
              >
                {error}
              </Alert>
            )}
            {state && errors.length === 0 && (
              <Alert
                isInline
                variant="info"
                className="metal2vm-spaced"
                title={t('What happens next')}
              >
                {t(
                  'metal2vm runs pre-flight checks first. Downtime starts only when the server boots the migration environment.',
                )}
              </Alert>
            )}
            {state && (
              <>
                <DescriptionList isHorizontal className="metal2vm-spaced">
                  <DescriptionListGroup>
                    <DescriptionListTerm>{t('Physical server')}</DescriptionListTerm>
                    <DescriptionListDescription>{state.source}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>{t('Boot')}</DescriptionListTerm>
                    <DescriptionListDescription>
                      {state.bootMethod === 'redfish'
                        ? t('Through its BMC (automatic)')
                        : state.bootMethod === 'pxe'
                          ? t('Network boot (PXE)')
                          : t('I boot it from its ISO myself (USB stick or virtual media)')}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>{t('Disks')}</DescriptionListTerm>
                    <DescriptionListDescription>
                      {state.disks.map((d) => {
                        const inv = src?.status?.inventory?.disks?.find((x) => x.id === d.id);
                        const what =
                          d.action === 'copy'
                            ? t('copy to {{sc}}', {
                                sc: (d.storageClassName ? d.storageClassName : defaultSC) ?? '-',
                              })
                            : d.action === 'attach'
                              ? t('re-attach SAN LUN')
                              : t('skip');
                        return (
                          <div key={d.id}>
                            {humanBytes(inv?.sizeBytes)} {inv?.model ?? d.id}: {what}
                          </div>
                        );
                      })}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>{t('Network')}</DescriptionListTerm>
                    <DescriptionListDescription>
                      {state.nics.map((n) => (
                        <div key={n.mac}>
                          {osNIC(n.mac)?.name ?? n.mac}:{' '}
                          {n.network === 'none'
                            ? t('not connected')
                            : n.network === 'pod'
                              ? t('pod network (new IP)')
                              : `${n.network}${n.keepIP ? `, ${t('keep IP')}` : ''}${n.keepMAC ? `, ${t('keep MAC')}` : ''}`}
                        </div>
                      ))}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>{t('Virtual machine')}</DescriptionListTerm>
                    <DescriptionListDescription>
                      {state.vmName || state.source}
                      {state.memory ? `, ${state.memory}` : ''}
                      {state.cores > 0 ? `, ${t('{{n}} cores', { n: state.cores })}` : ''}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>{t('Cutover')}</DescriptionListTerm>
                    <DescriptionListDescription>
                      {state.testBoot
                        ? t('Test boot first, then you cut over')
                        : t('Straight to production networks')}
                      {state.autoShutdownSource ? `; ${t('the server is powered off')}` : ''}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
                <ExpandableSection toggleText={t('Show the P2VMigration resource')}>
                  <CodeBlock>
                    <CodeBlockCode>{JSON.stringify(buildMigration(state), null, 2)}</CodeBlockCode>
                  </CodeBlock>
                </ExpandableSection>
              </>
            )}
          </WizardStep>
        </Wizard>
      </PageSection>
    </>
  );
};

export default MigrationWizard;
