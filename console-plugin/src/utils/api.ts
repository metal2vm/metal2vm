import { k8sCreate, k8sPatch } from '@openshift-console/dynamic-plugin-sdk';
import type { K8sModel, K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

export interface JSONPatch {
  op: 'add' | 'replace' | 'remove';
  path: string;
  value?: unknown;
}

export const createResource = <R extends K8sResourceCommon>(model: K8sModel, data: R): Promise<R> =>
  k8sCreate({ model, data });

export const patchResource = <R extends K8sResourceCommon>(
  model: K8sModel,
  resource: R,
  patch: JSONPatch[],
): Promise<R> => k8sPatch({ model, resource, data: patch });
