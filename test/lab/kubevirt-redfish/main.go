/*
Copyright 2026 The metal2vm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// kubevirt-redfish is a LAB TOOL: a Redfish BMC for KubeVirt VMs that play
// physical servers in metal2vm end-to-end tests. Each VM labelled
// metal2vm.io/role=lab-physical-server is a Redfish system:
//
//   - InsertMedia imports the ISO URL into a DataVolume and attaches it as
//     the VM's "live" CD-ROM; EjectMedia detaches it.
//   - A boot override puts the CD-ROM (Cd) or the "root" disk (Hdd) first.
//     Unlike a real BMC the override is persistent, not one-time.
//   - Reset uses the VM start/stop/restart subresources.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/metal2vm/metal2vm/internal/bmc/redfishsim"
)

const (
	roleLabel   = "metal2vm.io/role"
	roleValue   = "lab-physical-server"
	liveDisk    = "live"
	rootDisk    = "root"
	sourceAnnot = "lab.metal2vm.io/vmedia-url"
)

type backend struct {
	c         client.Client
	http      *http.Client
	host      string
	namespace string
	caCM      string
}

func (b *backend) vm(name string) (*kubevirtv1.VirtualMachine, error) {
	var vm kubevirtv1.VirtualMachine
	err := b.c.Get(context.Background(), types.NamespacedName{Namespace: b.namespace, Name: name}, &vm)
	return &vm, err
}

func (b *backend) Systems() []string {
	var list kubevirtv1.VirtualMachineList
	if err := b.c.List(context.Background(), &list, client.InNamespace(b.namespace),
		client.MatchingLabels{roleLabel: roleValue}); err != nil {
		log.Printf("listing VMs: %v", err)
		return nil
	}
	out := make([]string, 0, len(list.Items))
	for _, vm := range list.Items {
		out = append(out, vm.Name)
	}
	return out
}

func (b *backend) PowerOn(name string) (bool, error) {
	var vmi kubevirtv1.VirtualMachineInstance
	err := b.c.Get(context.Background(), types.NamespacedName{Namespace: b.namespace, Name: name}, &vmi)
	if client.IgnoreNotFound(err) != nil {
		return false, err
	}
	return err == nil && vmi.DeletionTimestamp == nil &&
		(vmi.Status.Phase == kubevirtv1.Running || vmi.Status.Phase == kubevirtv1.Scheduled ||
			vmi.Status.Phase == kubevirtv1.Scheduling || vmi.Status.Phase == kubevirtv1.Pending), nil
}

// subresource calls PUT .../virtualmachines/<name>/<action>.
func (b *backend) subresource(name, action string, body []byte) error {
	url := fmt.Sprintf("%s/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachines/%s/%s",
		b.host, b.namespace, name, action)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("%s %s: %s %s", action, name, resp.Status, buf.String())
	}
	return nil
}

func (b *backend) Reset(name, resetType string) error {
	on, err := b.PowerOn(name)
	if err != nil {
		return err
	}
	log.Printf("reset %s %s (on=%v)", name, resetType, on)
	switch resetType {
	case "On":
		if on {
			return nil
		}
		return b.subresource(name, "start", nil)
	case "ForceOff":
		if !on {
			return nil
		}
		return b.subresource(name, "stop", []byte(`{"gracePeriod":0}`))
	case "GracefulShutdown":
		if !on {
			return nil
		}
		return b.subresource(name, "stop", nil)
	case "ForceRestart", "GracefulRestart":
		if !on {
			return b.subresource(name, "start", nil)
		}
		return b.subresource(name, "restart", nil)
	}
	return fmt.Errorf("unsupported ResetType %q", resetType)
}

func (b *backend) SetBootOnce(name, target string) error {
	vm, err := b.vm(name)
	if err != nil {
		return err
	}
	first := rootDisk
	if target == "Cd" {
		first = liveDisk
	}
	log.Printf("boot %s %s", name, target)
	patch := client.MergeFrom(vm.DeepCopy())
	next := uint(2)
	for i := range vm.Spec.Template.Spec.Domain.Devices.Disks {
		d := &vm.Spec.Template.Spec.Domain.Devices.Disks[i]
		switch d.Name {
		case first:
			d.BootOrder = new(uint(1))
		case liveDisk, rootDisk:
			d.BootOrder = new(next)
			next++
		}
	}
	return b.c.Patch(context.Background(), vm, patch)
}

func (b *backend) InsertMedia(name, image string) error {
	vm, err := b.vm(name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(image))
	dvName := name + "-vmedia-" + hex.EncodeToString(sum[:])[:8]
	log.Printf("insert %s %s -> DataVolume %s", name, image, dvName)
	dv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{
			"name": dvName, "namespace": b.namespace, "annotations": map[string]any{sourceAnnot: image},
		},
		"spec": map[string]any{
			"source":  map[string]any{"http": map[string]any{"url": image, "certConfigMap": b.caCM}},
			"storage": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": "2Gi"}}},
		},
	}}
	if err := b.c.Create(context.Background(), dv); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	patch := client.MergeFrom(vm.DeepCopy())
	spec := &vm.Spec.Template.Spec
	removeLive(vm)
	spec.Domain.Devices.Disks = append(spec.Domain.Devices.Disks, kubevirtv1.Disk{
		Name: liveDisk, DiskDevice: kubevirtv1.DiskDevice{CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSATA}},
	})
	spec.Volumes = append(spec.Volumes, kubevirtv1.Volume{Name: liveDisk,
		VolumeSource: kubevirtv1.VolumeSource{DataVolume: &kubevirtv1.DataVolumeSource{Name: dvName}}})
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	vm.Annotations[sourceAnnot] = image
	return b.c.Patch(context.Background(), vm, patch)
}

// removeLive detaches the "live" CD-ROM, including a dataVolumeTemplate that
// only it referenced (KubeVirt rejects unreferenced templates).
func removeLive(vm *kubevirtv1.VirtualMachine) {
	spec := &vm.Spec.Template.Spec
	liveDV := ""
	for _, v := range spec.Volumes {
		if v.Name == liveDisk && v.DataVolume != nil {
			liveDV = v.DataVolume.Name
		}
	}
	if liveDV != "" {
		tmpls := vm.Spec.DataVolumeTemplates[:0]
		for _, t := range vm.Spec.DataVolumeTemplates {
			if t.Name != liveDV {
				tmpls = append(tmpls, t)
			}
		}
		vm.Spec.DataVolumeTemplates = tmpls
	}
	disks := spec.Domain.Devices.Disks[:0]
	for _, d := range spec.Domain.Devices.Disks {
		if d.Name != liveDisk {
			disks = append(disks, d)
		}
	}
	spec.Domain.Devices.Disks = disks
	vols := spec.Volumes[:0]
	for _, v := range spec.Volumes {
		if v.Name != liveDisk {
			vols = append(vols, v)
		}
	}
	spec.Volumes = vols
}

func (b *backend) EjectMedia(name string) error {
	vm, err := b.vm(name)
	if err != nil {
		return err
	}
	log.Printf("eject %s", name)
	patch := client.MergeFrom(vm.DeepCopy())
	removeLive(vm)
	delete(vm.Annotations, sourceAnnot)
	return b.c.Patch(context.Background(), vm, patch)
}

func (b *backend) Media(name string) (string, bool) {
	vm, err := b.vm(name)
	if err != nil {
		return "", false
	}
	for _, v := range vm.Spec.Template.Spec.Volumes {
		if v.Name == liveDisk {
			return vm.Annotations[sourceAnnot], true
		}
	}
	return "", false
}

func main() {
	addr := flag.String("listen", ":8000", "HTTP listen address")
	ns := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace of the lab VMs")
	caCM := flag.String("ca-configmap", "metal2vm-gateway-ca", "ConfigMap with the CA CDI uses to import ISOs")
	flag.Parse()

	cfg := ctrl.GetConfigOrDie()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = kubevirtv1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatal(err)
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		log.Fatal(err)
	}
	b := &backend{c: c, http: hc, host: cfg.Host, namespace: *ns, caCM: *caCM}
	srv := &redfishsim.Server{Backend: b, Username: os.Getenv("REDFISH_USERNAME"), Password: os.Getenv("REDFISH_PASSWORD"),
		Manufacturer: "metal2vm lab", Model: "KubeVirt VM (simulated server)"}
	log.Printf("kubevirt-redfish serving namespace %s on %s", *ns, *addr)
	s := &http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(s.ListenAndServe())
}
