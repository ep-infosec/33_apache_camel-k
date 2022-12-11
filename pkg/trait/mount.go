/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trait

import (
	"fmt"
	"path/filepath"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/pointer"

	serving "knative.dev/serving/pkg/apis/serving/v1"

	v1 "github.com/apache/camel-k/pkg/apis/camel/v1"
	traitv1 "github.com/apache/camel-k/pkg/apis/camel/v1/trait"
	"github.com/apache/camel-k/pkg/util/kubernetes"
	utilResource "github.com/apache/camel-k/pkg/util/resource"
)

type mountTrait struct {
	BaseTrait
	traitv1.MountTrait `property:",squash"`
}

func newMountTrait() Trait {
	return &mountTrait{
		// Must follow immediately the container trait
		BaseTrait: NewBaseTrait("mount", 1610),
	}
}

func (t *mountTrait) Configure(e *Environment) (bool, error) {
	if e.Integration == nil || !pointer.BoolDeref(t.Enabled, true) {
		return false, nil
	}

	if e.IntegrationInPhase(v1.IntegrationPhaseInitialization) ||
		(!e.IntegrationInPhase(v1.IntegrationPhaseInitialization) && !e.IntegrationInRunningPhases()) {
		return false, nil
	}

	// Validate resources and pvcs
	for _, c := range t.Configs {
		if !strings.HasPrefix(c, "configmap:") && !strings.HasPrefix(c, "secret:") {
			return false, fmt.Errorf("unsupported config %s, must be a configmap or secret resource", c)
		}
	}
	for _, r := range t.Resources {
		if !strings.HasPrefix(r, "configmap:") && !strings.HasPrefix(r, "secret:") {
			return false, fmt.Errorf("unsupported resource %s, must be a configmap or secret resource", r)
		}
	}

	return true, nil
}

func (t *mountTrait) Apply(e *Environment) error {
	if e.IntegrationInPhase(v1.IntegrationPhaseInitialization) {
		return nil
	}

	container := e.GetIntegrationContainer()
	if container == nil {
		return fmt.Errorf("unable to find integration container: %s", e.Integration.Name)
	}

	var volumes *[]corev1.Volume
	visited := false

	// Deployment
	if err := e.Resources.VisitDeploymentE(func(deployment *appsv1.Deployment) error {
		volumes = &deployment.Spec.Template.Spec.Volumes
		visited = true
		return nil
	}); err != nil {
		return err
	}

	// Knative Service
	if err := e.Resources.VisitKnativeServiceE(func(service *serving.Service) error {
		volumes = &service.Spec.ConfigurationSpec.Template.Spec.Volumes
		visited = true
		return nil
	}); err != nil {
		return err
	}

	// CronJob
	if err := e.Resources.VisitCronJobE(func(cron *batchv1.CronJob) error {
		volumes = &cron.Spec.JobTemplate.Spec.Template.Spec.Volumes
		visited = true
		return nil
	}); err != nil {
		return err
	}

	if visited {
		// Volumes declared in the Integration resources
		e.configureVolumesAndMounts(volumes, &container.VolumeMounts)
		// Volumes declared in the trait config/resource options
		err := t.configureVolumesAndMounts(e, volumes, &container.VolumeMounts)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *mountTrait) configureVolumesAndMounts(e *Environment, vols *[]corev1.Volume, mnts *[]corev1.VolumeMount) error {
	for _, c := range t.Configs {
		if conf, parseErr := utilResource.ParseConfig(c); parseErr == nil {
			t.attachResource(e, conf)
			t.mountResource(vols, mnts, conf)
		} else {
			return parseErr
		}
	}
	for _, r := range t.Resources {
		if res, parseErr := utilResource.ParseResource(r); parseErr == nil {
			t.attachResource(e, res)
			t.mountResource(vols, mnts, res)
		} else {
			return parseErr
		}
	}
	for _, v := range t.Volumes {
		if vol, parseErr := utilResource.ParseVolume(v); parseErr == nil {
			t.mountResource(vols, mnts, vol)
		} else {
			return parseErr
		}
	}

	return nil
}

// attachResource is in charge to filter the autogenerated configmap and attach to the Integration resources.
// The owner trait will be in charge to bind it accordingly.
func (t *mountTrait) attachResource(e *Environment, conf *utilResource.Config) {
	if conf.StorageType() == utilResource.StorageTypeConfigmap {
		// verify if it was autogenerated
		cm, err := kubernetes.GetUnstructured(e.Ctx, e.Client, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			conf.Name(), e.Integration.Namespace)
		if err == nil && cm != nil && cm.GetLabels()[kubernetes.ConfigMapAutogenLabel] == "true" {
			refCm := kubernetes.NewConfigMap(e.Integration.Namespace, conf.Name(), "", "", "", nil)
			e.Resources.Add(refCm)
		}
	}
}

func (t *mountTrait) mountResource(vols *[]corev1.Volume, mnts *[]corev1.VolumeMount, conf *utilResource.Config) {
	refName := kubernetes.SanitizeLabel(conf.Name())
	dstDir := conf.DestinationPath()
	dstFile := ""
	if conf.DestinationPath() != "" {
		if conf.Key() != "" {
			dstFile = filepath.Base(conf.DestinationPath())
		} else {
			dstFile = conf.Key()
		}
	}
	vol := getVolume(refName, string(conf.StorageType()), conf.Name(), conf.Key(), dstFile)
	mntPath := getMountPoint(conf.Name(), dstDir, string(conf.StorageType()), string(conf.ContentType()))
	readOnly := true
	if conf.StorageType() == utilResource.StorageTypePVC {
		readOnly = false
	}
	mnt := getMount(refName, mntPath, dstFile, readOnly)

	*vols = append(*vols, *vol)
	*mnts = append(*mnts, *mnt)
}

// IsPlatformTrait overrides base class method.
func (t *mountTrait) IsPlatformTrait() bool {
	return true
}