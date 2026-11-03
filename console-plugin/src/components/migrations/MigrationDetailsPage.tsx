import {
  DocumentTitle,
  ResourceLink,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Bullseye,
  Button,
  Card,
  CardBody,
  CardTitle,
  Flex,
  FlexItem,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  PageSection,
  Progress,
  ProgressStep,
  ProgressStepper,
  Spinner,
  Title,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { useState, type FC } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router';

import {
  P2VMigrationGVK,
  P2VMigrationModel,
  VirtualMachineGVK,
  type P2VMigration,
} from '../../models';
import { patchResource, type JSONPatch } from '../../utils/api';
import {
  canArchive,
  canCutOver,
  canRollBack,
  condition,
  copyPercent,
  humanBytes,
} from '../../utils/migration';
import { ConditionsTable, NS, PhaseLabel, PreflightAlerts } from '../shared';
import '../metal2vm.css';

// The steps an admin sees, and which phases belong to each.
const steps: { title: string; phases: string[] }[] = [
  { title: 'Pre-flight', phases: ['Pending', 'Preflight'] },
  { title: 'Boot', phases: ['Booting', 'Connected'] },
  { title: 'Copy', phases: ['Copying'] },
  { title: 'Convert', phases: ['Converting'] },
  { title: 'Test', phases: ['ReadyForTest', 'Testing'] },
  { title: 'Cut over', phases: ['CuttingOver'] },
  { title: 'Done', phases: ['Completed'] },
];

export const stepIndex = (phase?: string) => {
  const i = steps.findIndex((s) => s.phases.includes(phase ?? 'Pending'));
  return i < 0 ? steps.length - 1 : i;
};

type Action = 'cutover' | 'rollback' | 'archive';

const MigrationDetailsPage: FC = () => {
  const { t } = useTranslation(NS);
  const { ns = 'default', name = '' } = useParams();
  const [m, loaded, err] = useK8sWatchResource<P2VMigration>({
    groupVersionKind: P2VMigrationGVK,
    namespace: ns,
    name,
  }) as [P2VMigration, boolean, unknown];
  const [confirm, setConfirm] = useState<Action | null>(null);
  const [actionError, setActionError] = useState('');

  if (err) {
    return (
      <PageSection>
        <Alert isInline variant="danger" title={t('Unable to load the migration')}>
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

  const phase = m.status?.phase;
  const failed = phase === 'Failed';
  const current = stepIndex(phase);
  const degraded = condition(m.status?.conditions, 'Degraded');
  const vm = m.status?.vmName;

  const run = async (a: Action) => {
    setActionError('');
    const patch: JSONPatch[] =
      a === 'cutover'
        ? [
            {
              op: 'add',
              path: '/spec/cutover',
              value: new Date().toISOString().replace(/\.\d+Z$/, 'Z'),
            },
          ]
        : a === 'rollback'
          ? [{ op: 'add', path: '/spec/rollback', value: true }]
          : [{ op: 'add', path: '/spec/archived', value: true }];
    try {
      await patchResource(P2VMigrationModel, m, patch);
      setConfirm(null);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    }
  };

  const confirmText: Record<
    Action,
    { title: string; body: string; button: string; danger?: boolean }
  > = {
    cutover: {
      title: t('Cut over to the virtual machine?'),
      body: t(
        'The physical server is powered off and the VM restarts on its production networks. Until you archive the migration, you can still roll back.',
      ),
      button: t('Cut over'),
    },
    rollback: {
      title: t('Roll back to the physical server?'),
      body: t(
        'The VM is stopped and the physical server boots from its own disks again (automatically if it has a BMC). Its disks were never modified.',
      ),
      button: t('Roll back'),
      danger: true,
    },
    archive: {
      title: t('Archive the migration?'),
      body: t(
        'Temporary resources are removed. The VM and its disks stay. Rolling back is no longer possible.',
      ),
      button: t('Archive'),
    },
  };

  return (
    <>
      <DocumentTitle>{name}</DocumentTitle>
      <PageSection>
        <Flex justifyContent={{ default: 'justifyContentSpaceBetween' }}>
          <FlexItem>
            <Title headingLevel="h1">
              {name} <PhaseLabel phase={phase} />
            </Title>
            <Link to={`/p2v/ns/${ns}/sources/${m.spec.sourceRef.name}`}>
              {t('Physical server: {{name}}', { name: m.spec.sourceRef.name })}
            </Link>
          </FlexItem>
          <FlexItem>
            {canCutOver(m) && (
              <Button
                variant="primary"
                className="pf-v6-u-mr-sm"
                onClick={() => {
                  setConfirm('cutover');
                }}
              >
                {t('Cut over')}
              </Button>
            )}
            {canRollBack(m) && (
              <Button
                variant="secondary"
                isDanger
                className="pf-v6-u-mr-sm"
                onClick={() => {
                  setConfirm('rollback');
                }}
              >
                {t('Roll back')}
              </Button>
            )}
            {canArchive(m) && (
              <Button
                variant="secondary"
                onClick={() => {
                  setConfirm('archive');
                }}
              >
                {t('Archive')}
              </Button>
            )}
          </FlexItem>
        </Flex>

        <Card className="metal2vm-section">
          <CardBody>
            <ProgressStepper aria-label={t('Migration progress')}>
              {steps.map((s, i) => (
                <ProgressStep
                  key={s.title}
                  id={`step-${String(i)}`}
                  titleId={`step-${String(i)}-title`}
                  variant={
                    i < current || phase === 'Completed'
                      ? 'success'
                      : i === current
                        ? failed
                          ? 'danger'
                          : 'info'
                        : 'pending'
                  }
                  isCurrent={i === current}
                >
                  {t(s.title)}
                </ProgressStep>
              ))}
            </ProgressStepper>
          </CardBody>
        </Card>

        {m.status?.message && (
          <Alert
            isInline
            className="metal2vm-section"
            variant={failed ? 'danger' : phase === 'Testing' ? 'custom' : 'info'}
            title={m.status.message}
          >
            {phase === 'Testing' && vm && (
              <Link to={`/k8s/ns/${ns}/kubevirt.io~v1~VirtualMachine/${vm}/console`}>
                {t('Open the VM console')}
              </Link>
            )}
          </Alert>
        )}
        {degraded?.status === 'True' && (
          <Alert
            isInline
            variant="danger"
            className="metal2vm-section"
            title={t('A step is failing and being retried')}
          >
            {degraded.message}
          </Alert>
        )}
        {actionError && (
          <Alert isInline variant="danger" className="metal2vm-section" title={t('Action failed')}>
            {actionError}
          </Alert>
        )}
        <div className="metal2vm-section">
          <PreflightAlerts migration={m} />
        </div>

        {(m.status?.disks?.length ?? 0) > 0 && (
          <Card className="metal2vm-section">
            <CardTitle>{t('Disks')}</CardTitle>
            <CardBody>
              {phase === 'Copying' && (
                <Progress
                  value={copyPercent(m)}
                  title={t('Copying disks')}
                  className="metal2vm-spaced"
                />
              )}
              <Table aria-label={t('Disks')} variant="compact">
                <Thead>
                  <Tr>
                    <Th>{t('Source disk')}</Th>
                    <Th>{t('Status')}</Th>
                    <Th>{t('Copied')}</Th>
                    <Th>{t('Volume')}</Th>
                  </Tr>
                </Thead>
                <Tbody>
                  {(m.status?.disks ?? []).map((d) => (
                    <Tr key={d.sourceDiskID}>
                      <Td dataLabel={t('Source disk')}>{d.sourceDiskID}</Td>
                      <Td dataLabel={t('Status')}>{d.phase}</Td>
                      <Td dataLabel={t('Copied')}>
                        {d.bytesTotal
                          ? `${humanBytes(d.bytesCopied)} / ${humanBytes(d.bytesTotal)}`
                          : '-'}
                      </Td>
                      <Td dataLabel={t('Volume')}>{d.pvcName ?? '-'}</Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            </CardBody>
          </Card>
        )}

        {(vm !== undefined || m.status?.guest !== undefined) && (
          <Card className="metal2vm-section">
            <CardTitle>{t('Virtual machine')}</CardTitle>
            <CardBody>
              {vm && <ResourceLink groupVersionKind={VirtualMachineGVK} name={vm} namespace={ns} />}
              {m.status?.guest?.productName && (
                <p className="metal2vm-spaced">{m.status.guest.productName}</p>
              )}
              {(m.status?.guest?.networkNotes?.length ?? 0) > 0 && (
                <>
                  <Title headingLevel="h4">{t('Network configuration changes in the guest')}</Title>
                  <List>
                    {(m.status?.guest?.networkNotes ?? []).map((n) => (
                      <ListItem key={n}>{n}</ListItem>
                    ))}
                  </List>
                </>
              )}
            </CardBody>
          </Card>
        )}

        <Card className="metal2vm-section">
          <CardTitle>{t('Conditions')}</CardTitle>
          <CardBody>
            <ConditionsTable conditions={m.status?.conditions} />
          </CardBody>
        </Card>
      </PageSection>

      {confirm && (
        <Modal
          variant="small"
          isOpen
          onClose={() => {
            setConfirm(null);
          }}
          aria-labelledby="confirm-title"
        >
          <ModalHeader title={confirmText[confirm].title} labelId="confirm-title" />
          <ModalBody>{confirmText[confirm].body}</ModalBody>
          <ModalFooter>
            <Button
              variant={confirmText[confirm].danger ? 'danger' : 'primary'}
              onClick={() => void run(confirm)}
            >
              {confirmText[confirm].button}
            </Button>
            <Button
              variant="link"
              onClick={() => {
                setConfirm(null);
              }}
            >
              {t('Cancel')}
            </Button>
          </ModalFooter>
        </Modal>
      )}
    </>
  );
};

export default MigrationDetailsPage;
