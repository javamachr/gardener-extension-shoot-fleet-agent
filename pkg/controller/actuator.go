// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	projConfig "github.com/javamachr/gardener-extension-shoot-fleet-agent/pkg/apis/config"

	"github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/gardener/gardener/pkg/extensions"
	"github.com/go-logr/logr"
	fleetv1alpha1 "github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/extensions/pkg/controller/extension"
	"github.com/javamachr/gardener-extension-shoot-fleet-agent/pkg/controller/config"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
)

// ActuatorName is the name of the Fleet agent actuator.
const ActuatorName = "shoot-fleet-agent-actuator"

// KubeconfigSecretName name of secret that holds kubeconfig for Shoot
const KubeconfigSecretName = "kubecfg"

// KubeconfigKey key in KubeconfigSecretName secret that holds kubeconfig for Shoot
const KubeconfigKey = "kubeconfig"

// DefaultConfigKey is the name of default config key.
const DefaultConfigKey = "default"

// NewActuator returns an actuator responsible for Extension resources.
func NewActuator(config config.Config) extension.Actuator {
	logger := log.Log.WithName(ActuatorName)
	fleetManagers := initializeFleetManagers(config, logger)

	return &actuator{
		logger:        logger,
		serviceConfig: config,
		fleetManagers: fleetManagers,
	}
}

type actuator struct {
	client        client.Client
	config        *rest.Config
	decoder       runtime.Decoder
	fleetManagers map[string]*FleetManager

	serviceConfig config.Config

	logger logr.Logger
}

// initializeFleetManagers initializes fleet managers for given config
func initializeFleetManagers(config config.Config, logger logr.Logger) map[string]*FleetManager {
	fleetManagers := make(map[string]*FleetManager)
	fleetManagers[DefaultConfigKey] = createFleetManager(config.DefaultConfiguration, logger)
	for name, projConfig := range config.ProjectConfiguration {
		fleetManagers[name] = createFleetManager(projConfig, logger)
	}
	return fleetManagers
}

// Reconcile the Extension resource.
func (a *actuator) Reconcile(ctx context.Context, ex *extensionsv1alpha1.Extension) error {
	namespace := ex.GetNamespace()
	a.logger.Info("Component is being reconciled", "component", "fleet-agent-management", "namespace", namespace)
	cluster, err := controller.GetCluster(ctx, a.client, namespace)
	if err != nil {
		return err
	}
	if isShootedSeedCluster(cluster) {
		return a.updateStatus(ctx, ex)
	}
	shootsConfigOverride := &config.Config{}
	if ex.Spec.ProviderConfig != nil { //parse providerConfig defaults override for this Shoot
		if _, _, err := a.decoder.Decode(ex.Spec.ProviderConfig.Raw, nil, shootsConfigOverride); err != nil {
			return fmt.Errorf("failed to decode provider config: %+v", err)
		}
	}
	a.ReconcileClusterInFleetManager(ctx, namespace, cluster, shootsConfigOverride)
	return a.updateStatus(ctx, ex)
}

// Delete the Extension resource.
func (a *actuator) Delete(ctx context.Context, ex *extensionsv1alpha1.Extension) error {
	namespace := ex.GetNamespace()
	cluster, err := controller.GetCluster(ctx, a.client, namespace)
	if err != nil {
		return err
	}
	if isShootedSeedCluster(cluster) {
		return nil
	}
	a.logger.Info("Component is being deleted", "component", "fleet-agent-management", "namespace", namespace, "cluster", buildCrdName(cluster))
	err = a.getFleetManager(cluster).DeleteKubeconfigSecret(ctx, buildCrdName(cluster))
	if err != nil {
		a.logger.Error(err, "Failed to delete kubeconfig secret for Shoot cluster.", "cluster", buildCrdName(cluster))
	}
	err = a.getFleetManager(cluster).DeleteCluster(ctx, buildCrdName(cluster))
	if err != nil {
		a.logger.Error(err, "Failed to delete Cluster registration for Shoot cluster.", "cluster", buildCrdName(cluster))
	}
	return nil
}

// Restore the Extension resource.
func (a *actuator) Restore(ctx context.Context, ex *extensionsv1alpha1.Extension) error {
	//NOOP as there are no resources by this controller in Seed
	return nil
}

// Migrate the Extension resource.
func (a *actuator) Migrate(ctx context.Context, ex *extensionsv1alpha1.Extension) error {
	//NOOP as there are no resources by this controller in Seed
	return nil
}

// InjectConfig injects the rest config to this actuator.
func (a *actuator) InjectConfig(config *rest.Config) error {
	a.config = config
	return nil
}

// InjectClient injects the controller runtime client into the reconciler.
func (a *actuator) InjectClient(client client.Client) error {
	a.client = client
	return nil
}

// InjectScheme injects the given scheme into the reconciler.
func (a *actuator) InjectScheme(scheme *runtime.Scheme) error {
	a.decoder = serializer.NewCodecFactory(scheme).UniversalDecoder()
	return nil
}

// ReconcileClusterInFleetManager reconciles cluster registration in remote fleet manager
func (a *actuator) ReconcileClusterInFleetManager(ctx context.Context, namespace string, cluster *extensions.Cluster, override *config.Config) {
	a.logger.Info("Starting with already registered check")
	labels := prepareLabels(cluster, getProjectConfig(cluster, &a.serviceConfig), getProjectConfig(cluster, override))
	registered, err := a.getFleetManager(cluster).GetCluster(ctx, cluster.Shoot.Name)
	if err != nil {
		a.logger.Error(err, "Failed to get cluster registration for Shoot", "shoot", cluster.Shoot.Name)
	}
	if err == nil && registered != nil {
		if reflect.DeepEqual(registered.Labels, labels) {
			a.logger.Info("Cluster already registered - skipping registration", "clientId", registered.Spec.ClientID)
		} else {
			a.logger.Info("Updating labels of already registered cluster.", "clientId", registered.Spec.ClientID)
			a.updateClusterLabelsInFleet(ctx, registered, cluster, labels)
		}
		return
	}
	a.registerNewClusterInFleet(ctx, namespace, cluster, labels)
}

func (a *actuator) updateClusterLabelsInFleet(ctx context.Context, clusterRegistration *fleetv1alpha1.Cluster, cluster *extensions.Cluster, labels map[string]string) {
	clusterRegistration.Labels = labels
	_, err := a.getFleetManager(cluster).UpdateCluster(ctx, clusterRegistration)
	if err != nil {
		a.logger.Error(err, "Failed to update cluster labels in Fleet registration.", "clusterName", clusterRegistration.Name)
	}
}

func (a *actuator) registerNewClusterInFleet(ctx context.Context, namespace string, cluster *extensions.Cluster, labels map[string]string) {
	a.logger.Info("Looking up Secret with KubeConfig for given Shoot.", "namespace", namespace, "secretName", KubeconfigSecretName)
	secret := &corev1.Secret{}
	if err := a.client.Get(ctx, kutil.Key(namespace, KubeconfigSecretName), secret); err == nil {
		secretData := make(map[string][]byte)
		secretData["value"] = secret.Data[KubeconfigKey]
		a.logger.Info("Loaded kubeconfig from secret", "kubeconfig", secret, "namespace", namespace)

		const fleetRegisterNamespace = "clusters"
		kubeconfigSecret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kubecfg-" + buildCrdName(cluster),
				Namespace: fleetRegisterNamespace,
			},
			Data: secretData,
		}

		clusterRegistration := fleetv1alpha1.Cluster{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name:      buildCrdName(cluster),
				Namespace: fleetRegisterNamespace,
				Labels:    labels,
			},
			Spec: fleetv1alpha1.ClusterSpec{
				KubeConfigSecret: "kubecfg-" + buildCrdName(cluster),
			},
		}
		if _, err = a.getFleetManager(cluster).CreateKubeconfigSecret(ctx, &kubeconfigSecret); err != nil {
			a.logger.Error(err, "Failed to create secret with kubeconfig for Fleet registration")
		}
		if _, err = a.getFleetManager(cluster).CreateCluster(ctx, &clusterRegistration); err != nil {
			a.logger.Error(err, "Failed to create Cluster for Fleet registration")
		}
		a.logger.Info("Registered shoot cluster in Fleet Manager ", "registration", clusterRegistration)
	} else {
		a.logger.Error(err, "Failed to find Secret with kubeconfig for Fleet registration.")
	}
}

func prepareLabels(cluster *extensions.Cluster, serviceConfig projConfig.ProjectConfig, override projConfig.ProjectConfig) map[string]string {
	labels := make(map[string]string)
	labels["corebundle"] = "true"
	labels["region"] = cluster.Shoot.Spec.Region
	labels["cluster"] = cluster.Shoot.Name
	labels["seed"] = cluster.Seed.Name
	if len(override.Labels) > 0 { //adds labels from Shoot configuration
		for key, value := range override.Labels {
			labels[key] = value
		}
	} else {
		if len(serviceConfig.Labels) > 0 { //adds labels from default configuration
			for key, value := range serviceConfig.Labels {
				labels[key] = value
			}
		}
	}
	return labels
}

func (a *actuator) updateStatus(ctx context.Context, ex *extensionsv1alpha1.Extension) error {
	return controller.TryUpdateStatus(ctx, retry.DefaultBackoff, a.client, ex, func() error {
		return nil
	})
}

func (a *actuator) getFleetManager(cluster *extensions.Cluster) *FleetManager {
	manager, present := a.fleetManagers[getProjectName(cluster)]
	if !present {
		return a.fleetManagers[DefaultConfigKey]
	}
	return manager
}

// getProjectConfig return project specific or default config
func getProjectConfig(cluster *extensions.Cluster, serviceConfig *config.Config) projConfig.ProjectConfig {
	name := getProjectName(cluster)
	projectConfig, present := serviceConfig.ProjectConfiguration[name]
	if !present {
		return serviceConfig.DefaultConfiguration
	}
	return projectConfig
}

// buildCrdName creates a unique name for cluster registration resources in Fleet manager cluster
func buildCrdName(cluster *extensions.Cluster) string {
	return cluster.Seed.Name + "" + cluster.Shoot.Name
}

// isShootedSeedCluster checks if clusters purpose is Infrastructure
func isShootedSeedCluster(cluster *extensions.Cluster) bool {
	return *cluster.Shoot.Spec.Purpose == v1beta1.ShootPurposeInfrastructure
}

// getProjectName extracts project name from Shoots namespace
func getProjectName(cluster *extensions.Cluster) string {
	return cluster.Shoot.Namespace[strings.LastIndex(cluster.Shoot.Namespace, "-")+1:]
}
