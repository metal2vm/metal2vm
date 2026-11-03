import { DocumentTitle } from '@openshift-console/dynamic-plugin-sdk';
import {
  ActionGroup,
  Alert,
  Button,
  Checkbox,
  Form,
  FormGroup,
  FormHelperText,
  FormSection,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  PageSection,
  TextInput,
  Title,
} from '@patternfly/react-core';
import { useState, type FC } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router';

import { API_VERSION, P2VSourceModel, SecretModel, type P2VSource } from '../../models';
import { createResource } from '../../utils/api';
import { NS } from '../shared';
import '../metal2vm.css';

export interface SourceForm {
  name: string;
  useBMC: boolean;
  bmcAddress: string;
  bmcSystemID: string;
  bmcUser: string;
  bmcPassword: string;
  bmcInsecure: boolean;
  usePXE: boolean;
  pxeMAC: string;
  useOS: boolean;
  osType: 'ssh' | 'winrm';
  osAddress: string;
  osUser: string;
  osPassword: string;
}

const empty: SourceForm = {
  name: '',
  useBMC: false,
  bmcAddress: 'https://',
  bmcSystemID: '',
  bmcUser: '',
  bmcPassword: '',
  bmcInsecure: false,
  usePXE: false,
  pxeMAC: '',
  useOS: false,
  osType: 'ssh',
  osAddress: '',
  osUser: '',
  osPassword: '',
};

const nameRe = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const macRe = /^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$/;

/** Validation messages for the form; empty when it can be submitted. */
export const validateSource = (f: SourceForm): string[] => {
  const e: string[] = [];
  if (!nameRe.test(f.name)) e.push('name');
  if (f.useBMC && (!/^https?:\/\/.+/.test(f.bmcAddress) || !f.bmcUser || !f.bmcPassword))
    e.push('bmc');
  if (f.usePXE && !macRe.test(f.pxeMAC)) e.push('pxe');
  if (f.useOS && (!f.osAddress || !f.osUser || !f.osPassword)) e.push('os');
  return e;
};

/** buildSource returns the credentials Secrets and the P2VSource for the form. */
export const buildSource = (f: SourceForm, namespace: string) => {
  const secrets: {
    metadata: { name: string; namespace: string };
    stringData: Record<string, string>;
  }[] = [];
  const spec: P2VSource['spec'] = {};
  if (f.useBMC) {
    secrets.push({
      metadata: { name: `${f.name}-bmc`, namespace },
      stringData: { username: f.bmcUser, password: f.bmcPassword },
    });
    spec.bmc = {
      address: f.bmcAddress.replace(/\/$/, ''),
      credentialsSecretRef: { name: `${f.name}-bmc` },
      ...(f.bmcSystemID ? { systemID: f.bmcSystemID } : {}),
      ...(f.bmcInsecure ? { insecureSkipVerify: true } : {}),
    };
  }
  if (f.usePXE) spec.pxe = { bootMACAddress: f.pxeMAC.toLowerCase() };
  if (f.useOS) {
    secrets.push({
      metadata: { name: `${f.name}-os`, namespace },
      stringData: { username: f.osUser, password: f.osPassword },
    });
    spec.osAccess = {
      type: f.osType,
      address: f.osAddress,
      credentialsSecretRef: { name: `${f.name}-os` },
    };
  }
  const source: P2VSource = {
    apiVersion: API_VERSION,
    kind: P2VSourceModel.kind,
    metadata: { name: f.name, namespace },
    spec,
  };
  return { secrets, source };
};

const AddSourcePage: FC = () => {
  const { t } = useTranslation(NS);
  const { ns = 'default' } = useParams();
  const navigate = useNavigate();
  const [f, setF] = useState<SourceForm>(empty);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const set = <K extends keyof SourceForm>(k: K, v: SourceForm[K]) => {
    setF((s) => ({ ...s, [k]: v }));
  };
  const invalid = validateSource(f);

  const submit = async () => {
    setBusy(true);
    setError('');
    try {
      const { secrets, source } = buildSource(f, ns);
      for (const s of secrets) {
        await createResource(SecretModel, {
          apiVersion: 'v1',
          kind: 'Secret',
          type: 'Opaque',
          ...s,
        });
      }
      await createResource(P2VSourceModel, source);
      void navigate(`/p2v/ns/${ns}/sources/${f.name}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  return (
    <>
      <DocumentTitle>{t('Add physical server')}</DocumentTitle>
      <PageSection>
        <Title headingLevel="h1">{t('Add physical server')}</Title>
        <Form
          className="metal2vm-section metal2vm-form"
          onSubmit={(e) => {
            e.preventDefault();
          }}
        >
          <FormGroup label={t('Name')} isRequired fieldId="name">
            <TextInput
              id="name"
              value={f.name}
              onChange={(_e, v) => {
                set('name', v);
              }}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem variant={invalid.includes('name') && f.name ? 'error' : 'default'}>
                  {t('Lowercase letters, digits and "-", e.g. the server hostname.')}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>

          <FormSection title={t('How metal2vm can reach the server')}>
            <Checkbox
              id="use-bmc"
              label={t('BMC (Redfish: iDRAC, iLO, XClarity, ...)')}
              description={t(
                'Boots the migration environment and powers the server off and on remotely.',
              )}
              isChecked={f.useBMC}
              onChange={(_e, v) => {
                set('useBMC', v);
              }}
            />
            {f.useBMC && (
              <>
                <FormGroup label={t('BMC address')} isRequired fieldId="bmc-address">
                  <TextInput
                    id="bmc-address"
                    value={f.bmcAddress}
                    onChange={(_e, v) => {
                      set('bmcAddress', v);
                    }}
                  />
                </FormGroup>
                <FormGroup label={t('BMC user')} isRequired fieldId="bmc-user">
                  <TextInput
                    id="bmc-user"
                    value={f.bmcUser}
                    onChange={(_e, v) => {
                      set('bmcUser', v);
                    }}
                  />
                </FormGroup>
                <FormGroup label={t('BMC password')} isRequired fieldId="bmc-password">
                  <TextInput
                    id="bmc-password"
                    type="password"
                    value={f.bmcPassword}
                    onChange={(_e, v) => {
                      set('bmcPassword', v);
                    }}
                  />
                </FormGroup>
                <FormGroup
                  label={t('Redfish system ID (only if the BMC manages several systems)')}
                  fieldId="bmc-system"
                >
                  <TextInput
                    id="bmc-system"
                    value={f.bmcSystemID}
                    onChange={(_e, v) => {
                      set('bmcSystemID', v);
                    }}
                  />
                </FormGroup>
                <Checkbox
                  id="bmc-insecure"
                  label={t('Accept a self-signed BMC certificate')}
                  isChecked={f.bmcInsecure}
                  onChange={(_e, v) => {
                    set('bmcInsecure', v);
                  }}
                />
              </>
            )}
            <Checkbox
              id="use-pxe"
              label={t('Network boot (PXE)')}
              description={t('The server boots the migration environment from the network.')}
              isChecked={f.usePXE}
              onChange={(_e, v) => {
                set('usePXE', v);
              }}
            />
            {f.usePXE && (
              <FormGroup label={t('MAC address of the PXE boot NIC')} isRequired fieldId="pxe-mac">
                <TextInput
                  id="pxe-mac"
                  value={f.pxeMAC}
                  placeholder="3c:ec:ef:12:34:56"
                  onChange={(_e, v) => {
                    set('pxeMAC', v);
                  }}
                />
              </FormGroup>
            )}
            {!f.useBMC && !f.usePXE && (
              <Alert isInline variant="info" title={t('Manual boot')}>
                {t(
                  'Without a BMC or PXE, you boot the server from its metal2vm ISO (USB stick or virtual media) yourself.',
                )}
              </Alert>
            )}
          </FormSection>

          <FormSection title={t('Running operating system (optional)')}>
            <Checkbox
              id="use-os"
              label={t('Run a read-only pre-check of the running OS')}
              description={t(
                'Reads the hardware, interface names and IP settings before any downtime, so static IPs can be kept.',
              )}
              isChecked={f.useOS}
              onChange={(_e, v) => {
                set('useOS', v);
              }}
            />
            {f.useOS && (
              <>
                <FormGroup label={t('Operating system')} fieldId="os-type">
                  <FormSelect
                    id="os-type"
                    value={f.osType}
                    onChange={(_e, v) => {
                      set('osType', v as 'ssh' | 'winrm');
                    }}
                  >
                    <FormSelectOption value="ssh" label={t('Linux (SSH)')} />
                    <FormSelectOption value="winrm" label={t('Windows (WinRM)')} />
                  </FormSelect>
                </FormGroup>
                <FormGroup label={t('Hostname or IP address')} isRequired fieldId="os-address">
                  <TextInput
                    id="os-address"
                    value={f.osAddress}
                    onChange={(_e, v) => {
                      set('osAddress', v);
                    }}
                  />
                </FormGroup>
                <FormGroup label={t('User')} isRequired fieldId="os-user">
                  <TextInput
                    id="os-user"
                    value={f.osUser}
                    onChange={(_e, v) => {
                      set('osUser', v);
                    }}
                  />
                </FormGroup>
                <FormGroup label={t('Password')} isRequired fieldId="os-password">
                  <TextInput
                    id="os-password"
                    type="password"
                    value={f.osPassword}
                    onChange={(_e, v) => {
                      set('osPassword', v);
                    }}
                  />
                </FormGroup>
              </>
            )}
          </FormSection>

          {error && (
            <Alert isInline variant="danger" title={t('Could not add the server')}>
              {error}
            </Alert>
          )}
          <ActionGroup>
            <Button
              variant="primary"
              isDisabled={invalid.length > 0 || busy}
              isLoading={busy}
              onClick={() => void submit()}
            >
              {t('Add')}
            </Button>
            <Button variant="link" onClick={() => void navigate(-1)}>
              {t('Cancel')}
            </Button>
          </ActionGroup>
        </Form>
      </PageSection>
    </>
  );
};

export default AddSourcePage;
