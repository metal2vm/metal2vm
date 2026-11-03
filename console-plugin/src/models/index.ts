import type {
  K8sGroupVersionKind,
  K8sModel,
  K8sResourceCommon,
} from '@openshift-console/dynamic-plugin-sdk';

// Mirrors api/v1alpha1 (migration.metal2vm.io); only fields the UI uses.

const group = 'migration.metal2vm.io';
const version = 'v1alpha1';

/** apiVersion of metal2vm resources. */
export const API_VERSION = `${group}/${version}`;

export const P2VMigrationGVK: K8sGroupVersionKind = { group, version, kind: 'P2VMigration' };
export const P2VSourceGVK: K8sGroupVersionKind = { group, version, kind: 'P2VSource' };
export const VirtualMachineGVK: K8sGroupVersionKind = {
  group: 'kubevirt.io',
  version: 'v1',
  kind: 'VirtualMachine',
};
export const StorageClassGVK: K8sGroupVersionKind = {
  group: 'storage.k8s.io',
  version: 'v1',
  kind: 'StorageClass',
};
export const NADGVK: K8sGroupVersionKind = {
  group: 'k8s.cni.cncf.io',
  version: 'v1',
  kind: 'NetworkAttachmentDefinition',
};
export const SecretGVK: K8sGroupVersionKind = { version: 'v1', kind: 'Secret' };

const model = (kind: string, plural: string, abbr: string): K8sModel => ({
  apiGroup: group,
  apiVersion: version,
  kind,
  plural,
  abbr,
  label: kind,
  labelPlural: `${kind}s`,
  namespaced: true,
  crd: true,
});

export const P2VMigrationModel = model('P2VMigration', 'p2vmigrations', 'P2VM');
export const P2VSourceModel = model('P2VSource', 'p2vsources', 'P2VS');
export const SecretModel: K8sModel = {
  apiVersion: 'v1',
  kind: 'Secret',
  plural: 'secrets',
  abbr: 'S',
  label: 'Secret',
  labelPlural: 'Secrets',
  namespaced: true,
};

export interface Condition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

export interface Disk {
  id: string;
  path?: string;
  sizeBytes: number;
  model?: string;
  serial?: string;
  wwn?: string;
  transport?: string;
  multipath?: boolean;
  encrypted?: boolean;
}

export interface NIC {
  mac: string;
  name?: string;
  linkSpeedMbps?: number;
  ipAddresses?: string[];
  gateway?: string;
  dnsServers?: string[];
  dhcp?: boolean;
}

export interface Inventory {
  collectedAt: string;
  collectedBy: 'PreCheck' | 'LiveAgent';
  firmware?: { type?: 'bios' | 'uefi'; secureBoot?: boolean; tpm?: boolean };
  cpu?: { model?: string; sockets?: number; coresPerSocket?: number; threadsPerCore?: number };
  memoryMiB?: number;
  disks?: Disk[];
  nics?: NIC[];
  warnings?: string[];
}

export interface OSConfig {
  collectedAt: string;
  os?: { family?: string; distribution?: string; version?: string; hostname?: string };
  nics?: NIC[];
  warnings?: string[];
}

export type P2VSource = K8sResourceCommon & {
  spec: {
    bmc?: {
      address: string;
      systemID?: string;
      credentialsSecretRef: { name: string };
      insecureSkipVerify?: boolean;
      mediaProtocol?: 'HTTPS' | 'HTTP';
    };
    pxe?: { bootMACAddress: string };
    osAccess?: {
      type: 'ssh' | 'winrm';
      address: string;
      port?: number;
      credentialsSecretRef: { name: string };
      insecureSkipVerify?: boolean;
    };
  };
  status?: {
    inventory?: Inventory;
    osConfig?: OSConfig;
    bmc?: { model?: string; manufacturer?: string; serialNumber?: string; powerState?: string };
    bootMedia?: { secretName: string; expiresAt: string };
    agent?: { connected?: boolean; lastHeartbeat?: string; version?: string };
    conditions?: Condition[];
  };
};

export type MigrationPhase =
  | 'Pending'
  | 'Preflight'
  | 'Booting'
  | 'Connected'
  | 'Copying'
  | 'Converting'
  | 'ReadyForTest'
  | 'Testing'
  | 'CuttingOver'
  | 'Completed'
  | 'RollingBack'
  | 'RolledBack'
  | 'Failed';

export type DiskAction = 'copy' | 'attach' | 'skip';
export type BootMethod = 'manual' | 'redfish' | 'pxe';

export interface DiskMapping {
  sourceDiskID: string;
  action: DiskAction;
  storageClassName?: string;
  lun?: {
    iscsi?: { targetPortal: string; iqn: string; lun: number };
    fc?: { wwids?: string[] };
  };
}

export interface NICMapping {
  sourceMAC: string;
  type: 'pod' | 'multus';
  networkName?: string;
  keepMAC?: boolean;
  keepIP?: boolean;
}

export type P2VMigration = K8sResourceCommon & {
  spec: {
    sourceRef: { name: string };
    mode?: 'cold' | 'warm';
    bootMethod?: BootMethod;
    targetVM?: {
      name?: string;
      cpu?: { sockets?: number; cores?: number; threads?: number };
      memory?: string;
    };
    disks: DiskMapping[];
    nics?: NICMapping[];
    testBoot?: boolean;
    autoShutdownSource?: boolean;
    cutover?: string;
    rollback?: boolean;
    archived?: boolean;
  };
  status?: {
    phase?: MigrationPhase;
    message?: string;
    vmName?: string;
    startedAt?: string;
    completedAt?: string;
    guest?: { family?: string; osinfo?: string; productName?: string; networkNotes?: string[] };
    disks?: {
      sourceDiskID: string;
      pvcName?: string;
      bytesTotal?: number;
      bytesCopied?: number;
      phase?: string;
    }[];
    preflight?: {
      name: string;
      passed: boolean;
      severity: 'Blocker' | 'Warning' | 'Info';
      message?: string;
    }[];
    conditions?: Condition[];
  };
};

export type StorageClass = K8sResourceCommon & {
  provisioner?: string;
};

export type NetworkAttachmentDefinition = K8sResourceCommon & { spec?: { config?: string } };
