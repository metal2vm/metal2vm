import { buildSource, validateSource, type SourceForm } from './AddSourcePage';

const base: SourceForm = {
  name: 'db01',
  useBMC: true,
  bmcAddress: 'https://10.0.0.21/',
  bmcSystemID: '',
  bmcUser: 'root',
  bmcPassword: 'calvin',
  bmcInsecure: true,
  usePXE: false,
  pxeMAC: '',
  useOS: true,
  osType: 'winrm',
  osAddress: 'db01.example.com',
  osUser: 'Administrator',
  osPassword: 'pw',
};

describe('add physical server', () => {
  it('creates one Secret per access method and references them', () => {
    const { secrets, source } = buildSource(base, 'apps');
    expect(secrets.map((s) => s.metadata.name)).toEqual(['db01-bmc', 'db01-os']);
    expect(secrets[0].stringData).toEqual({ username: 'root', password: 'calvin' });
    expect(source.spec.bmc).toEqual({
      address: 'https://10.0.0.21',
      credentialsSecretRef: { name: 'db01-bmc' },
      insecureSkipVerify: true,
    });
    expect(source.spec.osAccess).toEqual({
      type: 'winrm',
      address: 'db01.example.com',
      credentialsSecretRef: { name: 'db01-os' },
    });
    expect(source.spec.pxe).toBeUndefined();
  });

  it('validates every enabled section', () => {
    expect(validateSource(base)).toEqual([]);
    expect(validateSource({ ...base, name: 'DB 01' })).toContain('name');
    expect(validateSource({ ...base, bmcPassword: '' })).toContain('bmc');
    expect(validateSource({ ...base, usePXE: true, pxeMAC: 'nope' })).toContain('pxe');
    expect(validateSource({ ...base, usePXE: true, pxeMAC: '3C:EC:EF:00:00:01' })).toEqual([]);
  });
});
