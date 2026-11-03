import type { P2VMigration, P2VSource } from '../models';
import {
  buildMigration,
  canArchive,
  canCutOver,
  canRollBack,
  copyPercent,
  humanBytes,
  initialState,
  uniqueName,
  validate,
} from './migration';

const source: P2VSource = {
  metadata: { name: 'web01', namespace: 'apps' },
  spec: { bmc: { address: 'https://10.0.0.1', credentialsSecretRef: { name: 'web01-bmc' } } },
  status: {
    inventory: {
      collectedAt: '2026-11-03T00:00:00Z',
      collectedBy: 'LiveAgent',
      disks: [
        { id: 'root', sizeBytes: 100 },
        { id: 'data', sizeBytes: 200 },
      ],
      nics: [{ mac: '3c:ec:ef:00:00:01' }, { mac: '3c:ec:ef:00:00:02' }],
    },
  },
};

describe('wizard mapping', () => {
  it('avoids the names of existing migrations', () => {
    expect(uniqueName('web01', [])).toBe('web01');
    expect(uniqueName('web01', ['web01', 'web01-2'])).toBe('web01-3');
    const s = initialState('apps', source, ['web01']);
    expect(s.name).toBe('web01-2');
    expect(buildMigration(s).spec.targetVM?.name).toBe('web01-2');
  });

  it('starts with every disk copied, the first NIC on the pod network and BMC boot', () => {
    const s = initialState('apps', source);
    expect(s.bootMethod).toBe('redfish');
    expect(s.disks.map((d) => d.action)).toEqual(['copy', 'copy']);
    expect(s.nics.map((n) => n.network)).toEqual(['pod', 'none']);
    expect(validate(s)).toEqual([]);
  });

  it('builds a minimal migration from the defaults', () => {
    const m = buildMigration(initialState('apps', source));
    expect(m.metadata).toEqual({ name: 'web01', namespace: 'apps' });
    expect(m.spec.sourceRef.name).toBe('web01');
    expect(m.spec.targetVM).toBeUndefined();
    expect(m.spec.disks).toEqual([
      { sourceDiskID: 'root', action: 'copy' },
      { sourceDiskID: 'data', action: 'copy' },
    ]);
    expect(m.spec.nics).toEqual([{ sourceMAC: '3c:ec:ef:00:00:01', type: 'pod' }]);
    expect(m.spec.testBoot).toBe(true);
  });

  it('maps storage classes, LUN re-attach, bridged NICs and VM overrides', () => {
    const s = initialState('apps', source);
    s.vmName = 'web01-vm';
    s.memory = '16Gi';
    s.cores = 4;
    s.disks[0].storageClassName = 'fast';
    s.disks[1] = {
      id: 'data',
      action: 'attach',
      storageClassName: '',
      iscsi: { targetPortal: '10.0.0.5:3260', iqn: 'iqn.x', lun: 2 },
    };
    s.nics[0] = { mac: '3c:ec:ef:00:00:01', network: 'vlan100', keepMAC: true, keepIP: true };
    const m = buildMigration(s);
    expect(m.spec.targetVM).toEqual({
      name: 'web01-vm',
      memory: '16Gi',
      cpu: { sockets: 1, cores: 4 },
    });
    expect(m.spec.disks[0]).toEqual({
      sourceDiskID: 'root',
      action: 'copy',
      storageClassName: 'fast',
    });
    expect(m.spec.disks[1]).toEqual({
      sourceDiskID: 'data',
      action: 'attach',
      lun: { iscsi: { targetPortal: '10.0.0.5:3260', iqn: 'iqn.x', lun: 2 } },
    });
    expect(m.spec.nics).toEqual([
      {
        sourceMAC: '3c:ec:ef:00:00:01',
        type: 'multus',
        networkName: 'vlan100',
        keepMAC: true,
        keepIP: true,
      },
    ]);
  });

  it('reports what blocks creation', () => {
    const s = initialState('apps', source);
    s.name = 'Bad_Name';
    s.disks.forEach((d) => (d.action = 'skip'));
    s.nics.forEach((n) => (n.network = 'pod'));
    s.memory = 'lots';
    const errs = validate(s);
    expect(errs).toHaveLength(4);
  });
});

describe('migration helpers', () => {
  const m = (phase: string, extra: Partial<P2VMigration['spec']> = {}) =>
    ({
      metadata: {},
      spec: { sourceRef: { name: 's' }, disks: [], ...extra },
      status: { phase },
    }) as P2VMigration;

  it('offers actions only where they make sense', () => {
    expect(canCutOver(m('Testing'))).toBe(true);
    expect(canCutOver(m('Testing', { cutover: '2026-11-03T00:00:00Z' }))).toBe(false);
    expect(canCutOver(m('Copying'))).toBe(false);
    expect(canRollBack(m('Completed'))).toBe(true);
    expect(canRollBack(m('Completed', { archived: true }))).toBe(false);
    expect(canRollBack(m('Copying'))).toBe(false);
    expect(canArchive(m('RolledBack'))).toBe(true);
    expect(canArchive(m('Testing'))).toBe(false);
  });

  it('computes copy progress and readable sizes', () => {
    const mig = m('Copying');
    mig.status = {
      phase: 'Copying',
      disks: [
        { sourceDiskID: 'a', bytesTotal: 100, bytesCopied: 50 },
        { sourceDiskID: 'b', bytesTotal: 300, bytesCopied: 0 },
      ],
    };
    expect(copyPercent(mig)).toBe(12);
    expect(humanBytes(35 * 1024 ** 3)).toBe('35.0 GiB');
    expect(humanBytes(512)).toBe('512 B');
  });
});
