import type { K8sGroupVersionKind, K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

export const P2VMigrationGVK: K8sGroupVersionKind = {
  group: 'migration.metal2vm.io',
  version: 'v1alpha1',
  kind: 'P2VMigration',
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

// Mirrors api/v1alpha1/p2vmigration_types.go; only the fields the UI reads.
export type P2VMigration = K8sResourceCommon & {
  spec: {
    sourceRef: { name: string };
    mode?: 'cold' | 'warm';
    bootMethod?: 'redfish' | 'pxe' | 'manual';
  };
  status?: {
    phase?: MigrationPhase;
    message?: string;
    vmName?: string;
  };
};
