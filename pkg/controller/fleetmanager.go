package controller

import (
	"context"
	clientset "github.com/javamachr/gardener-extension-shoot-fleet-agent/pkg/client/fleet/clientset/versioned"
	"github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type FleetManager struct {
	secretClient kubernetes.Clientset
	fleetClient clientset.Interface
	namespace string
}

func NewManagerForConfig(c *rest.Config, namespace string) (*FleetManager, error) {
	secretClient, err := kubernetes.NewForConfig(c)
	if err != nil {
		return nil, err
	}

	fleetClient, err := clientset.NewForConfig(c)
	if err != nil {
		return nil, err
	}

	return &FleetManager{
		secretClient: *secretClient,
		fleetClient:  fleetClient,
		namespace: namespace,
	}, nil
}

func (f *FleetManager) CreateCluster(ctx context.Context, cluster *v1alpha1.Cluster) (*v1alpha1.Cluster, error) {
	return f.fleetClient.FleetV1alpha1().Clusters(f.namespace).Create(ctx, cluster, metav1.CreateOptions{})
}

func (f *FleetManager) UpdateCluster(ctx context.Context, cluster *v1alpha1.Cluster) (*v1alpha1.Cluster, error) {
	return f.fleetClient.FleetV1alpha1().Clusters(f.namespace).Update(ctx, cluster, metav1.UpdateOptions{})
}

func (f *FleetManager) GetCluster(ctx context.Context, clusterName string) (*v1alpha1.Cluster, error) {
	return f.fleetClient.FleetV1alpha1().Clusters(f.namespace).Get(ctx, clusterName, metav1.GetOptions{})
}

func (f *FleetManager) CreateKubeconfigSecret(ctx context.Context, secret *corev1.Secret) (*corev1.Secret, error) {
	return f.secretClient.CoreV1().Secrets(f.namespace).Create(ctx, secret, metav1.CreateOptions{})
}