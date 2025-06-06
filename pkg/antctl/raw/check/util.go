// Copyright 2024 Antrea Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package check

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func NewClient() (client kubernetes.Interface, config *rest.Config, clusterName string, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	nonInteractiveClient := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	config, err = nonInteractiveClient.ClientConfig()
	if err != nil {
		return nil, nil, "", err
	}
	rawConfig, err := nonInteractiveClient.RawConfig()
	if err != nil {
		return nil, nil, "", err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, "", err
	}
	contextName := rawConfig.CurrentContext
	clusterName = ""
	if context, ok := rawConfig.Contexts[contextName]; ok {
		clusterName = context.Cluster
	}
	return clientset, config, clusterName, nil
}

func DeploymentIsReady(ctx context.Context, client kubernetes.Interface, namespace, deploymentName string) (bool, error) {
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if deployment.Generation <= deployment.Status.ObservedGeneration {
		for _, cond := range deployment.Status.Conditions {
			if cond.Type == appsv1.DeploymentProgressing && cond.Reason == "ProgressDeadlineExceeded" {
				return false, fmt.Errorf("deployment %q exceeded its progress deadline", deployment.Name)
			}
		}
		if deployment.Spec.Replicas != nil && deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas {
			return false, nil
		}
		if deployment.Status.Replicas > deployment.Status.UpdatedReplicas {
			return false, nil
		}
		if deployment.Status.AvailableReplicas < deployment.Status.UpdatedReplicas {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

func NewDeployment(p DeploymentParameters) *appsv1.Deployment {
	if p.Replicas == 0 {
		p.Replicas = 1
	}
	replicas32 := int32(p.Replicas)
	var ports []corev1.ContainerPort
	if p.Port > 0 {
		ports = append(ports, corev1.ContainerPort{ContainerPort: int32(p.Port)})
	}
	var env []corev1.EnvVar
	if p.Port > 0 {
		env = append(env, corev1.EnvVar{Name: "PORT", Value: fmt.Sprintf("%d", p.Port)})
	}
	if p.Labels == nil {
		p.Labels = make(map[string]string)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   p.Name,
			Labels: p.Labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas32,
			Selector: &metav1.LabelSelector{
				MatchLabels: p.Labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: p.Labels,
				},
				Spec: corev1.PodSpec{
					HostNetwork:  p.HostNetwork,
					NodeSelector: p.NodeSelector,
					Containers: []corev1.Container{
						{
							Name:            p.Name,
							Image:           p.Image,
							Ports:           ports,
							Env:             env,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         p.Command,
							Args:            p.Args,
							VolumeMounts:    p.VolumeMounts,
							SecurityContext: p.SecurityContext,
						},
					},
					Tolerations: p.Tolerations,
					Volumes:     p.Volumes,
					Affinity:    p.Affinity,
				},
			},
		},
	}
}

type DeploymentParameters struct {
	Name            string
	Role            string
	Image           string
	Replicas        int
	Port            int
	Command         []string
	Args            []string
	Affinity        *corev1.Affinity
	Tolerations     []corev1.Toleration
	Labels          map[string]string
	VolumeMounts    []corev1.VolumeMount
	Volumes         []corev1.Volume
	HostNetwork     bool
	NodeSelector    map[string]string
	SecurityContext *corev1.SecurityContext
}

func WaitForDeploymentsReady(ctx context.Context,
	interval, timeout time.Duration,
	client kubernetes.Interface,
	clusterName string,
	namespace string,
	deployments ...string) error {
	for _, deployment := range deployments {
		fmt.Fprintf(os.Stdout, fmt.Sprintf("[%s] ", clusterName)+"Waiting for Deployment %s to become ready...\n", deployment)
		err := wait.PollUntilContextTimeout(ctx, interval, timeout, false, func(ctx context.Context) (bool, error) {
			ready, err := DeploymentIsReady(ctx, client, namespace, deployment)
			if err != nil {
				return false, fmt.Errorf("error checking readiness of Deployment %s: %w", deployment, err)
			}
			return ready, nil
		})
		if err != nil {
			return fmt.Errorf("waiting for Deployment %s to become ready has been interrupted: %w", deployment, err)
		}
		fmt.Fprintf(os.Stdout, fmt.Sprintf("[%s] ", clusterName)+"Deployment %s is ready.\n", deployment)
	}
	return nil
}

func GenerateRandomNamespace(baseName string) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	bytes := make([]byte, 5)
	_, err := rand.Read(bytes)
	if err != nil {
		panic(err)
	}
	for i, b := range bytes {
		bytes[i] = letters[b%byte(len(letters))]
	}
	return fmt.Sprintf("%s-%s", baseName, string(bytes))
}

func Teardown(ctx context.Context, logger Logger, client kubernetes.Interface, namespace string) {
	logger.Log("Deleting installation tests setup...")
	err := client.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err != nil {
		logger.Fail("Namespace %s deletion failed: %v", namespace, err)
		return
	}
	logger.Log("Waiting for Namespace %s to be deleted", namespace)
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 1*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		logger.Fail("Setup deletion failed")
	} else {
		logger.Success("Setup deletion successful")
	}
}
