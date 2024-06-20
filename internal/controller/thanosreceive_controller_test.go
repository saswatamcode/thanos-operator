/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	monitoringthanosiov1alpha1 "github.com/thanos-community/thanos-operator/api/v1alpha1"
	"github.com/thanos-community/thanos-operator/internal/pkg/manifests"
	"github.com/thanos-community/thanos-operator/internal/pkg/manifests/receive"
	"github.com/thanos-community/thanos-operator/test/utils"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

var _ = Describe("ThanosReceive Controller", Ordered, func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName = "test-resource"
			ns           = "treceive"

			objStoreSecretName = "test-secret"
			objStoreSecretKey  = "test-key.yaml"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: ns,
		}

		BeforeAll(func() {
			By("creating the namespace")
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: ns,
				},
			})).Should(Succeed())
		})

		BeforeEach(func() {
			By("creating the object store secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      objStoreSecretName,
					Namespace: ns,
				},
				StringData: map[string]string{
					objStoreSecretKey: `type: S3
config:
  bucket: "thanos"
  access_key: "thanos"
  secret_key: "thanos-secret"
  endpoint: "minio.treceive.svc:9000"
  insecure: true
  trace:
    enable: false`,
				},
			}

			err := k8sClient.Create(ctx, secret)
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterEach(func() {
			resource := &monitoringthanosiov1alpha1.ThanosReceive{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ThanosReceive")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should error when the spec is invalid due to CEL rules", func() {
			resource := &monitoringthanosiov1alpha1.ThanosReceive{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: ns,
				},
				Spec: monitoringthanosiov1alpha1.ThanosReceiveSpec{
					CommonThanosFields: monitoringthanosiov1alpha1.CommonThanosFields{},
					Router: monitoringthanosiov1alpha1.RouterSpec{
						Labels:            map[string]string{"test": "my-router-test"},
						ReplicationFactor: 3,
					},
					Ingester: monitoringthanosiov1alpha1.IngesterSpec{
						DefaultObjectStorageConfig: monitoringthanosiov1alpha1.ObjectStorageConfig{
							LocalObjectReference: corev1.LocalObjectReference{Name: "test-secret"},
							Key:                  "test-key",
						},
						Hashrings: []monitoringthanosiov1alpha1.IngestorHashringSpec{
							{
								Name:        "test-hashring",
								Labels:      map[string]string{"test": "my-ingester-test"},
								StorageSize: "100Mi",
								Tenants:     []string{"test-tenant"},
								Replicas:    2,
							},
						},
					},
				},
			}
			By("failing when the receive replica count is less than the ingester replication factor", func() {
				Expect(k8sClient.Create(context.Background(), resource)).ShouldNot(Succeed())
			})

			By("ensuring hashring name is a singleton across the list", func() {
				resource.Spec.Ingester.Hashrings[0].Replicas = 3
				Expect(k8sClient.Create(context.Background(), resource)).Should(Succeed())
				resource := &monitoringthanosiov1alpha1.ThanosReceive{}
				err := k8sClient.Get(ctx, typeNamespacedName, resource)
				Expect(err).NotTo(HaveOccurred())
				resource.Spec.Ingester.Hashrings = append(
					resource.Spec.Ingester.Hashrings,
					monitoringthanosiov1alpha1.IngestorHashringSpec{
						Name:        "test-hashring",
						Labels:      map[string]string{"test": "my-ingester-test"},
						StorageSize: "100Mi",
						Tenants:     []string{"test-tenant"},
						Replicas:    5,
					},
				)
				Expect(k8sClient.Update(context.Background(), resource)).ShouldNot(Succeed())
				resource.Spec.Ingester.Hashrings[1].Name = "test-hashring-2"
				Expect(k8sClient.Update(context.Background(), resource)).Should(Succeed())
			})
		})

		It("should reconcile correctly", func() {
			resource := &monitoringthanosiov1alpha1.ThanosReceive{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: ns,
				},
				Spec: monitoringthanosiov1alpha1.ThanosReceiveSpec{
					CommonThanosFields: monitoringthanosiov1alpha1.CommonThanosFields{},
					Router: monitoringthanosiov1alpha1.RouterSpec{
						Labels:            map[string]string{"test": "my-router-test"},
						ReplicationFactor: 3,
					},
					Ingester: monitoringthanosiov1alpha1.IngesterSpec{
						DefaultObjectStorageConfig: monitoringthanosiov1alpha1.ObjectStorageConfig{
							LocalObjectReference: corev1.LocalObjectReference{Name: "test-secret"},
							Key:                  "test-key",
						},
						Hashrings: []monitoringthanosiov1alpha1.IngestorHashringSpec{
							{
								Name:        "test-hashring",
								Labels:      map[string]string{"test": "my-ingester-test"},
								StorageSize: "100Mi",
								Tenants:     []string{"test-tenant"},
								Replicas:    3,
							},
						},
					},
				},
			}

			By("setting up the thanos receive ingest resources", func() {
				Expect(k8sClient.Create(context.Background(), resource)).Should(Succeed())

				name := receive.IngesterNameFromParent(resourceName, "test-hashring")
				Eventually(func() error {
					return validateExistenceOfRequiredNamedResources(expectApiResourceStatefulSet, name, ns)
				}, time.Minute*2, time.Second*10).Should(Succeed())

			})

			By("creating a hashring config in ConfigMap of the same name as the CR", func() {
				Eventually(func() bool {
					return utils.VerifyConfigMapContents(k8sClient, resourceName, ns, receive.HashringConfigKey, receive.EmptyHashringConfig)
				}, time.Minute*1, time.Second*10).Should(BeTrue())
			})

			By("reacting to the creation of a matching endpoint slice by updating the ConfigMap", func() {
				// we label below for verbosity in testing but via predicates we really should not need to deal
				// with such events. we do however want to ensure we deal with them correctly in case someone
				// was to add a label.
				epSliceNotRelevant := &discoveryv1.EndpointSlice{
					TypeMeta: metav1.TypeMeta{
						Kind:       "EndpointSlice",
						APIVersion: "discovery.k8s.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "no-onwer-ref",
						Namespace: ns,
						Labels:    map[string]string{manifests.ComponentLabel: receive.IngestComponentName},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
				}
				Expect(k8sClient.Create(context.Background(), epSliceNotRelevant)).Should(Succeed())

				epSliceNotRelevantNotRelevantService := &discoveryv1.EndpointSlice{
					TypeMeta: metav1.TypeMeta{
						Kind:       "EndpointSlice",
						APIVersion: "discovery.k8s.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-ep-slice-1",
						Namespace: ns,
						OwnerReferences: []metav1.OwnerReference{
							{
								Name:       "svc",
								Kind:       "Service",
								APIVersion: "v1",
								UID:        types.UID("1234"),
							},
						},
						Labels: map[string]string{manifests.ComponentLabel: receive.IngestComponentName},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
				}
				Expect(k8sClient.Create(context.Background(), epSliceNotRelevantNotRelevantService)).Should(Succeed())
				// check via a poll that we have not updated the ConfigMap
				Consistently(func() bool {
					return utils.VerifyConfigMapContents(k8sClient, resourceName, ns, receive.HashringConfigKey, receive.EmptyHashringConfig)
				}, time.Second*10, time.Second*1).Should(BeTrue())

				epSliceRelevant := &discoveryv1.EndpointSlice{
					TypeMeta: metav1.TypeMeta{
						Kind:       "EndpointSlice",
						APIVersion: "discovery.k8s.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-ep-slice-2",
						Namespace: ns,
						OwnerReferences: []metav1.OwnerReference{
							{
								Name:       receive.IngesterNameFromParent(resourceName, "test-hashring"),
								Kind:       "Service",
								APIVersion: "v1",
								UID:        types.UID("1234"),
							},
						},
						Labels: map[string]string{
							discoveryv1.LabelServiceName: receive.IngesterNameFromParent(resourceName, "test-hashring"),
							manifests.ComponentLabel:     receive.IngestComponentName},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"8.8.8.8"},
							Hostname:  ptr.To("some-hostname"),
							Conditions: discoveryv1.EndpointConditions{
								Ready:       ptr.To(true),
								Serving:     ptr.To(true),
								Terminating: ptr.To(false),
							},
						},
						{
							Addresses: []string{"1.1.1.1"},
							Hostname:  ptr.To("some-hostname-b"),
							Conditions: discoveryv1.EndpointConditions{
								Ready:       ptr.To(true),
								Serving:     ptr.To(true),
								Terminating: ptr.To(false),
							},
						},
						{
							Addresses: []string{"2.2.2.2"},
							Hostname:  ptr.To("some-hostname-c"),
							Conditions: discoveryv1.EndpointConditions{
								Ready:       ptr.To(true),
								Serving:     ptr.To(true),
								Terminating: ptr.To(false),
							},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), epSliceRelevant)).Should(Succeed())

				expect := `[
    {
        "hashring": "test-hashring",
        "tenants": [
            "test-tenant"
        ],
        "tenant_matcher_type": "exact",
        "endpoints": [
            {
                "address": "some-hostname-b.test-resource-test-hashring.treceive.svc.cluster.local:19291",
                "az": ""
            },
            {
                "address": "some-hostname-c.test-resource-test-hashring.treceive.svc.cluster.local:19291",
                "az": ""
            },
            {
                "address": "some-hostname.test-resource-test-hashring.treceive.svc.cluster.local:19291",
                "az": ""
            }
        ]
    }
]`

				Eventually(func() bool {
					return utils.VerifyConfigMapContents(k8sClient, resourceName, ns, receive.HashringConfigKey, expect)
				}, time.Minute*1, time.Second*2).Should(BeTrue())

			})
		})

	})
})

type expectApiResource string

const (
	expectApiResourceDeployment  expectApiResource = "Deployment"
	expectApiResourceStatefulSet expectApiResource = "StatefulSet"
)

func validateExistenceOfRequiredNamedResources(expectResource expectApiResource, name, ns string) error {
	sa := &corev1.ServiceAccount{}
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: ns,
	}, sa); err != nil {
		return err
	}

	svc := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: ns,
	}, svc); err != nil {
		return err
	}

	switch expectResource {
	case expectApiResourceDeployment:
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: ns,
		}, dep); err != nil {
			return err
		}
	case expectApiResourceStatefulSet:
		sts := &appsv1.StatefulSet{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: ns,
		}, sts); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unexpected resource type")
	}
	return nil
}
