import type {
  BootMethod,
  DiskAction,
  MigrationPhase,
  NICMapping,
  P2VMigration,
  P2VSource,
} from '../models';
import { API_VERSION, P2VMigrationModel } from '../models';

// Wizard state, kept independent of the API so the mapping is testable.
export interface DiskChoice {
  id: string;
  action: DiskAction;
  storageClassName: string;
  iscsi?: { targetPortal: string; iqn: string; lun: number };
}

export interface NICChoice {
  mac: string;
  // 'pod', 'none', or the name of a NetworkAttachmentDefinition.
  network: string;
  keepMAC: boolean;
  keepIP: boolean;
}

export interface WizardState {
  name: string;
  namespace: string;
  source: string;
  bootMethod: BootMethod;
  vmName: string;
  memory: string; // e.g. "16Gi"; empty keeps the source's size
  cores: number; // 0 keeps the source's topology
  disks: DiskChoice[];
  nics: NICChoice[];
  testBoot: boolean;
  autoShutdownSource: boolean;
}

/** uniqueName returns base, or base-2, base-3, ... if base is taken. */
export const uniqueName = (base: string, taken: string[]): string => {
  if (!taken.includes(base)) return base;
  let i = 2;
  while (taken.includes(`${base}-${String(i)}`)) i++;
  return `${base}-${String(i)}`;
};

/**
 * Initial wizard state for a source: copy every disk, pod network for the
 * first NIC. takenNames are existing migrations, avoided by the default name.
 */
export const initialState = (
  namespace: string,
  src: P2VSource,
  takenNames: string[] = [],
): WizardState => {
  const disks = src.status?.inventory?.disks ?? [];
  const nics = src.status?.inventory?.nics ?? [];
  const bootMethod: BootMethod = src.spec.bmc ? 'redfish' : src.spec.pxe ? 'pxe' : 'manual';
  const name = uniqueName(src.metadata?.name ?? '', takenNames);
  return {
    name,
    namespace,
    source: src.metadata?.name ?? '',
    bootMethod,
    vmName: name,
    memory: '',
    cores: 0,
    disks: disks.map((d) => ({ id: d.id, action: 'copy', storageClassName: '' })),
    nics: nics.map((n, i) => ({
      mac: n.mac,
      network: i === 0 ? 'pod' : 'none',
      keepMAC: false,
      keepIP: false,
    })),
    testBoot: true,
    autoShutdownSource: true,
  };
};

/** Problems that must be fixed before the migration can be created. */
export const validate = (s: WizardState): string[] => {
  const errs: string[] = [];
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(s.name)) {
    errs.push(
      'The migration name must be a valid Kubernetes name (lowercase letters, digits, "-").',
    );
  }
  if (s.vmName && !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(s.vmName)) {
    errs.push('The VM name must be a valid Kubernetes name.');
  }
  if (!s.disks.some((d) => d.action === 'copy' || d.action === 'attach')) {
    errs.push('Choose at least one disk to copy or attach.');
  }
  for (const d of s.disks) {
    if (d.action === 'attach' && (!d.iscsi?.targetPortal || !d.iscsi.iqn)) {
      errs.push(`Disk ${d.id}: enter the iSCSI portal and IQN of the LUN to re-attach.`);
    }
  }
  if (s.nics.filter((n) => n.network === 'pod').length > 1) {
    errs.push('Only one NIC can use the pod network.');
  }
  if (s.memory && !/^[0-9]+(Mi|Gi)$/.test(s.memory)) {
    errs.push('Memory must look like 4096Mi or 16Gi.');
  }
  return errs;
};

/** buildMigration turns wizard state into a P2VMigration. */
export const buildMigration = (s: WizardState): P2VMigration => {
  const nics: NICMapping[] = s.nics
    .filter((n) => n.network !== 'none')
    .map((n) =>
      n.network === 'pod'
        ? { sourceMAC: n.mac, type: 'pod' }
        : {
            sourceMAC: n.mac,
            type: 'multus',
            networkName: n.network,
            keepMAC: n.keepMAC || undefined,
            keepIP: n.keepIP || undefined,
          },
    );
  const targetVM: NonNullable<P2VMigration['spec']['targetVM']> = {};
  if (s.vmName && s.vmName !== s.source) targetVM.name = s.vmName;
  if (s.memory) targetVM.memory = s.memory;
  if (s.cores > 0) targetVM.cpu = { sockets: 1, cores: s.cores };
  return {
    apiVersion: API_VERSION,
    kind: P2VMigrationModel.kind,
    metadata: { name: s.name, namespace: s.namespace },
    spec: {
      sourceRef: { name: s.source },
      mode: 'cold',
      bootMethod: s.bootMethod,
      ...(Object.keys(targetVM).length > 0 ? { targetVM } : {}),
      disks: s.disks.map((d) => ({
        sourceDiskID: d.id,
        action: d.action,
        ...(d.action === 'copy' && d.storageClassName
          ? { storageClassName: d.storageClassName }
          : {}),
        ...(d.action === 'attach' && d.iscsi ? { lun: { iscsi: d.iscsi } } : {}),
      })),
      ...(nics.length > 0 ? { nics } : {}),
      testBoot: s.testBoot,
      autoShutdownSource: s.autoShutdownSource,
    },
  };
};

const activePhases: MigrationPhase[] = [
  'Pending',
  'Preflight',
  'Booting',
  'Connected',
  'Copying',
  'Converting',
  'CuttingOver',
  'RollingBack',
];

export const isActive = (p?: MigrationPhase) => !p || activePhases.includes(p);
export const canCutOver = (m: P2VMigration) => m.status?.phase === 'Testing' && !m.spec.cutover;
export const canRollBack = (m: P2VMigration) =>
  !m.spec.archived &&
  !m.spec.rollback &&
  ['Testing', 'CuttingOver', 'Completed'].includes(m.status?.phase ?? '');
export const canArchive = (m: P2VMigration) =>
  !m.spec.archived && ['Completed', 'RolledBack', 'Failed'].includes(m.status?.phase ?? '');

export const condition = (conds: { type: string }[] | undefined, type: string) =>
  conds?.find((c) => c.type === type) as
    | { type: string; status: string; reason?: string; message?: string }
    | undefined;

/** Copy progress across all disks, 0..100. */
export const copyPercent = (m: P2VMigration): number => {
  const disks = m.status?.disks ?? [];
  const total = disks.reduce((a, d) => a + (d.bytesTotal ?? 0), 0);
  const done = disks.reduce((a, d) => a + (d.bytesCopied ?? 0), 0);
  return total > 0 ? Math.floor((done * 100) / total) : 0;
};

export const humanBytes = (n?: number): string => {
  if (!n) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i === 0 ? String(v) : v.toFixed(1)} ${units[i]}`;
};
