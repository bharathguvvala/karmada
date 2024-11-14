/*
Copyright 2021 The Karmada Authors.

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

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1alpha1 "github.com/karmada-io/karmada/pkg/apis/cluster/v1alpha1"
	policyv1alpha1 "github.com/karmada-io/karmada/pkg/apis/policy/v1alpha1"
	controllercluster "github.com/karmada-io/karmada/pkg/controllers/cluster"
	"github.com/karmada-io/karmada/pkg/util"
	"github.com/karmada-io/karmada/pkg/util/helper"
	"github.com/karmada-io/karmada/test/e2e/framework"
	testhelper "github.com/karmada-io/karmada/test/helper"
)

// failover testing is used to test the rescheduling situation when some initially scheduled clusters fail
var _ = framework.SerialDescribe("failover testing", func() {
	ginkgo.Context("Deployment propagation testing", func() {
		var policyNamespace, policyName string
		var deploymentNamespace, deploymentName string
		var deployment *appsv1.Deployment
		var maxGroups, minGroups, numOfFailedClusters int
		var policy *policyv1alpha1.PropagationPolicy

		ginkgo.BeforeEach(func() {
			policyNamespace = testNamespace
			policyName = deploymentNamePrefix + rand.String(RandomStrLength)
			deploymentNamespace = testNamespace
			deploymentName = policyName
			deployment = testhelper.NewDeployment(deploymentNamespace, deploymentName)
			maxGroups = 1
			minGroups = 1
			numOfFailedClusters = 1

			// set MaxGroups=MinGroups=1, label is location=CHN.
			policy = testhelper.NewPropagationPolicy(policyNamespace, policyName, []policyv1alpha1.ResourceSelector{
				{
					APIVersion: deployment.APIVersion,
					Kind:       deployment.Kind,
					Name:       deployment.Name,
				},
			}, policyv1alpha1.Placement{
				ClusterAffinity: &policyv1alpha1.ClusterAffinity{
					LabelSelector: &metav1.LabelSelector{
						// only test push mode clusters
						// because pull mode clusters cannot be disabled by changing APIEndpoint
						MatchLabels: pushModeClusterLabels,
					},
				},
				ClusterTolerations: []corev1.Toleration{
					*helper.NewNotReadyToleration(2),
					*helper.NewUnreachableToleration(2),
				},
				SpreadConstraints: []policyv1alpha1.SpreadConstraint{
					{
						SpreadByField: policyv1alpha1.SpreadByFieldCluster,
						MaxGroups:     maxGroups,
						MinGroups:     minGroups,
					},
				},
			})
		})

		ginkgo.BeforeEach(func() {
			framework.CreatePropagationPolicy(karmadaClient, policy)
			framework.CreateDeployment(kubeClient, deployment)
			ginkgo.DeferCleanup(func() {
				framework.RemoveDeployment(kubeClient, deployment.Namespace, deployment.Name)
				framework.RemovePropagationPolicy(karmadaClient, policy.Namespace, policy.Name)
			})
		})

		ginkgo.It("deployment failover testing", func() {
			var disabledClusters []string
			targetClusterNames := framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)

			ginkgo.By("set one cluster condition status to false", func() {
				temp := numOfFailedClusters
				for _, targetClusterName := range targetClusterNames {
					if temp > 0 {
						klog.Infof("Set cluster %s to disable.", targetClusterName)
						err := disableCluster(controlPlaneClient, targetClusterName)
						gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

						// wait for the current cluster status changing to false
						framework.WaitClusterFitWith(controlPlaneClient, targetClusterName, func(cluster *clusterv1alpha1.Cluster) bool {
							return helper.TaintExists(cluster.Spec.Taints, controllercluster.NotReadyTaintTemplate)
						})
						disabledClusters = append(disabledClusters, targetClusterName)
						temp--
					}
				}
			})

			ginkgo.By("check whether deployment of failed cluster is rescheduled to other available cluster", func() {
				gomega.Eventually(func() int {
					targetClusterNames = framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
					for _, targetClusterName := range targetClusterNames {
						// the target cluster should be overwritten to another available cluster
						if !testhelper.IsExclude(targetClusterName, disabledClusters) {
							return 0
						}
					}

					return len(targetClusterNames)
				}, pollTimeout, pollInterval).Should(gomega.Equal(minGroups))
			})

			ginkgo.By("recover not ready cluster", func() {
				for _, disabledCluster := range disabledClusters {
					fmt.Printf("cluster %s is waiting for recovering\n", disabledCluster)
					originalAPIEndpoint := getClusterAPIEndpoint(disabledCluster)

					err := recoverCluster(controlPlaneClient, disabledCluster, originalAPIEndpoint)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					// wait for the disabled cluster recovered
					err = wait.PollUntilContextTimeout(context.TODO(), pollInterval, pollTimeout, true, func(_ context.Context) (done bool, err error) {
						currentCluster, err := util.GetCluster(controlPlaneClient, disabledCluster)
						if err != nil {
							return false, err
						}
						if !helper.TaintExists(currentCluster.Spec.Taints, controllercluster.NotReadyTaintTemplate) {
							fmt.Printf("cluster %s recovered\n", disabledCluster)
							return true, nil
						}
						return false, nil
					})
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				}
			})

			ginkgo.By("check whether the deployment disappears in the recovered clusters", func() {
				framework.WaitDeploymentDisappearOnClusters(disabledClusters, deploymentNamespace, deploymentName)
			})
		})
	})

	ginkgo.Context("Taint cluster testing", func() {
		var policyNamespace, policyName string
		var deploymentNamespace, deploymentName string
		var deployment *appsv1.Deployment
		var taint corev1.Taint
		var maxGroups, minGroups, numOfFailedClusters int
		var policy *policyv1alpha1.PropagationPolicy
		maxGroups = 1
		minGroups = 1
		numOfFailedClusters = 1

		ginkgo.BeforeEach(func() {
			policyNamespace = testNamespace
			policyName = deploymentNamePrefix + rand.String(RandomStrLength)
			deploymentNamespace = testNamespace
			deploymentName = policyName
			deployment = testhelper.NewDeployment(deploymentNamespace, deploymentName)

			policy = testhelper.NewPropagationPolicy(policyNamespace, policyName, []policyv1alpha1.ResourceSelector{
				{
					APIVersion: deployment.APIVersion,
					Kind:       deployment.Kind,
					Name:       deployment.Name,
				},
			}, policyv1alpha1.Placement{
				ClusterAffinity: &policyv1alpha1.ClusterAffinity{
					ClusterNames: framework.ClusterNames(),
				},
				ClusterTolerations: []corev1.Toleration{
					{
						Key:               "fail-test",
						Effect:            corev1.TaintEffectNoExecute,
						Operator:          corev1.TolerationOpExists,
						TolerationSeconds: ptr.To[int64](3),
					},
				},
				SpreadConstraints: []policyv1alpha1.SpreadConstraint{
					{
						SpreadByField: policyv1alpha1.SpreadByFieldCluster,
						MaxGroups:     maxGroups,
						MinGroups:     minGroups,
					},
				},
			})

			taint = corev1.Taint{
				Key:    "fail-test",
				Effect: corev1.TaintEffectNoExecute,
			}
		})

		ginkgo.BeforeEach(func() {
			framework.CreatePropagationPolicy(karmadaClient, policy)
			framework.CreateDeployment(kubeClient, deployment)
			ginkgo.DeferCleanup(func() {
				framework.RemoveDeployment(kubeClient, deployment.Namespace, deployment.Name)
				framework.RemovePropagationPolicy(karmadaClient, policy.Namespace, policy.Name)
			})
		})

		ginkgo.It("taint cluster", func() {
			var disabledClusters []string
			targetClusterNames := framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
			ginkgo.By("taint one cluster", func() {
				temp := numOfFailedClusters
				for _, targetClusterName := range targetClusterNames {
					if temp > 0 {
						klog.Infof("Taint one cluster(%s).", targetClusterName)
						err := taintCluster(controlPlaneClient, targetClusterName, taint)
						gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
						disabledClusters = append(disabledClusters, targetClusterName)
						temp--
					}
				}
			})

			ginkgo.By("check whether deployment of taint cluster is rescheduled to other available cluster", func() {
				gomega.Eventually(func() int {
					targetClusterNames = framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
					for _, targetClusterName := range targetClusterNames {
						// the target cluster should be overwritten to another available cluster
						if !testhelper.IsExclude(targetClusterName, disabledClusters) {
							return 0
						}
					}

					return len(targetClusterNames)
				}, pollTimeout, pollInterval).Should(gomega.Equal(minGroups))
			})

			ginkgo.By("recover not ready cluster", func() {
				for _, disabledCluster := range disabledClusters {
					fmt.Printf("cluster %s is waiting for recovering\n", disabledCluster)
					err := recoverTaintedCluster(controlPlaneClient, disabledCluster, taint)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				}
			})

			ginkgo.By("check whether the deployment disappears in the recovered clusters", func() {
				framework.WaitDeploymentDisappearOnClusters(disabledClusters, deploymentNamespace, deploymentName)
			})
		})
	})

	ginkgo.Context("Application failover testing with purgeMode graciously", func() {
		var policyNamespace, policyName string
		var deploymentNamespace, deploymentName string
		var deployment *appsv1.Deployment
		var policy *policyv1alpha1.PropagationPolicy
		var overridePolicy *policyv1alpha1.OverridePolicy
		var maxGroups, minGroups int
		var gracePeriodSeconds, tolerationSeconds int32
		ginkgo.BeforeEach(func() {
			policyNamespace = testNamespace
			policyName = deploymentNamePrefix + rand.String(RandomStrLength)
			deploymentNamespace = testNamespace
			deploymentName = policyName
			deployment = testhelper.NewDeployment(deploymentNamespace, deploymentName)
			maxGroups = 1
			minGroups = 1
			gracePeriodSeconds = 30
			tolerationSeconds = 30

			policy = &policyv1alpha1.PropagationPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: policyNamespace,
					Name:      policyName,
				},
				Spec: policyv1alpha1.PropagationSpec{
					ResourceSelectors: []policyv1alpha1.ResourceSelector{
						{
							APIVersion: deployment.APIVersion,
							Kind:       deployment.Kind,
							Name:       deployment.Name,
						},
					},
					Placement: policyv1alpha1.Placement{
						ClusterAffinity: &policyv1alpha1.ClusterAffinity{
							ClusterNames: framework.ClusterNames(),
						},
						SpreadConstraints: []policyv1alpha1.SpreadConstraint{
							{
								SpreadByField: policyv1alpha1.SpreadByFieldCluster,
								MaxGroups:     maxGroups,
								MinGroups:     minGroups,
							},
						},
					},
					PropagateDeps: true,
					Failover: &policyv1alpha1.FailoverBehavior{
						Application: &policyv1alpha1.ApplicationFailoverBehavior{
							DecisionConditions: policyv1alpha1.DecisionConditions{
								TolerationSeconds: ptr.To[int32](tolerationSeconds),
							},
							PurgeMode:          policyv1alpha1.Graciously,
							GracePeriodSeconds: ptr.To[int32](gracePeriodSeconds),
						},
					},
				},
			}
		})

		ginkgo.BeforeEach(func() {
			framework.CreatePropagationPolicy(karmadaClient, policy)
			framework.CreateDeployment(kubeClient, deployment)
			ginkgo.DeferCleanup(func() {
				framework.RemoveDeployment(kubeClient, deployment.Namespace, deployment.Name)
				framework.RemovePropagationPolicy(karmadaClient, policy.Namespace, policy.Name)
			})
		})

		ginkgo.It("application failover with purgeMode graciously when the application come back to healthy on the new cluster", func() {
			disabledClusters := framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
			ginkgo.By("create an error op", func() {
				overridePolicy = testhelper.NewOverridePolicyByOverrideRules(policyNamespace, policyName, []policyv1alpha1.ResourceSelector{
					{
						APIVersion: deployment.APIVersion,
						Kind:       deployment.Kind,
						Name:       deployment.Name,
					},
				}, []policyv1alpha1.RuleWithCluster{
					{
						TargetCluster: &policyv1alpha1.ClusterAffinity{
							ClusterNames: disabledClusters,
						},
						Overriders: policyv1alpha1.Overriders{
							ImageOverrider: []policyv1alpha1.ImageOverrider{
								{
									Component: "Registry",
									Operator:  policyv1alpha1.OverriderOpReplace,
									Value:     "fake",
								},
							},
						},
					},
				})
				framework.CreateOverridePolicy(karmadaClient, overridePolicy)
			})

			ginkgo.By("check if deployment present on member clusters has correct image value", func() {
				framework.WaitDeploymentPresentOnClustersFitWith(disabledClusters, deployment.Namespace, deployment.Name,
					func(deployment *appsv1.Deployment) bool {
						for _, container := range deployment.Spec.Template.Spec.Containers {
							if container.Image != "fake/nginx:1.19.0" {
								return false
							}
						}
						return true
					})
			})

			ginkgo.By("check whether the failed deployment disappears in the disabledClusters", func() {
				framework.WaitDeploymentDisappearOnClusters(disabledClusters, deploymentNamespace, deploymentName)
			})

			ginkgo.By("check whether the failed deployment is rescheduled to other available cluster", func() {
				gomega.Eventually(func() int {
					targetClusterNames := framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
					for _, targetClusterName := range targetClusterNames {
						// the target cluster should be overwritten to another available cluster
						if !testhelper.IsExclude(targetClusterName, disabledClusters) {
							return 0
						}
					}

					return len(targetClusterNames)
				}, pollTimeout, pollInterval).Should(gomega.Equal(minGroups))
			})

			ginkgo.By("delete the error op", func() {
				framework.RemoveOverridePolicy(karmadaClient, policyNamespace, policyName)
			})
		})

		ginkgo.It("application failover with purgeMode graciously when the GracePeriodSeconds is reach out", func() {
			gracePeriodSeconds = 10
			ginkgo.By("update pp", func() {
				// modify gracePeriodSeconds to create a time difference with tolerationSecond to avoid cluster interference
				patch := []map[string]interface{}{
					{
						"op":    policyv1alpha1.OverriderOpReplace,
						"path":  "/spec/failover/application/gracePeriodSeconds",
						"value": ptr.To[int32](gracePeriodSeconds),
					},
				}
				framework.PatchPropagationPolicy(karmadaClient, policy.Namespace, policy.Name, patch, types.JSONPatchType)
			})

			disabledClusters := framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
			var beginTime time.Time
			ginkgo.By("create an error op", func() {
				overridePolicy = testhelper.NewOverridePolicyByOverrideRules(policyNamespace, policyName, []policyv1alpha1.ResourceSelector{
					{
						APIVersion: deployment.APIVersion,
						Kind:       deployment.Kind,
						Name:       deployment.Name,
					},
				}, []policyv1alpha1.RuleWithCluster{
					{
						TargetCluster: &policyv1alpha1.ClusterAffinity{
							// guarantee that application cannot come back to healthy on the new cluster
							ClusterNames: framework.ClusterNames(),
						},
						Overriders: policyv1alpha1.Overriders{
							ImageOverrider: []policyv1alpha1.ImageOverrider{
								{
									Component: "Registry",
									Operator:  policyv1alpha1.OverriderOpReplace,
									Value:     "fake",
								},
							},
						},
					},
				})
				framework.CreateOverridePolicy(karmadaClient, overridePolicy)
				beginTime = time.Now()
			})
			defer framework.RemoveOverridePolicy(karmadaClient, policyNamespace, policyName)

			ginkgo.By("check if deployment present on member clusters has correct image value", func() {
				framework.WaitDeploymentPresentOnClustersFitWith(disabledClusters, deployment.Namespace, deployment.Name,
					func(deployment *appsv1.Deployment) bool {
						for _, container := range deployment.Spec.Template.Spec.Containers {
							if container.Image != "fake/nginx:1.19.0" {
								return false
							}
						}
						return true
					})
			})

			ginkgo.By("check whether application failover with purgeMode graciously when the GracePeriodSeconds is reach out", func() {
				framework.WaitDeploymentDisappearOnClusters(disabledClusters, deploymentNamespace, deploymentName)
				evictionTime := time.Now()
				gomega.Expect(evictionTime.Sub(beginTime) > time.Duration(gracePeriodSeconds+tolerationSeconds)*time.Second).Should(gomega.BeTrue())
			})
		})
	})

	ginkgo.Context("Application failover testing with purgeMode never", func() {
		var policyNamespace, policyName string
		var deploymentNamespace, deploymentName string
		var deployment *appsv1.Deployment
		var policy *policyv1alpha1.PropagationPolicy
		var overridePolicy *policyv1alpha1.OverridePolicy
		var maxGroups, minGroups int
		ginkgo.BeforeEach(func() {
			policyNamespace = testNamespace
			policyName = deploymentNamePrefix + rand.String(RandomStrLength)
			deploymentNamespace = testNamespace
			deploymentName = policyName
			deployment = testhelper.NewDeployment(deploymentNamespace, deploymentName)
			maxGroups = 1
			minGroups = 1

			policy = &policyv1alpha1.PropagationPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: policyNamespace,
					Name:      policyName,
				},
				Spec: policyv1alpha1.PropagationSpec{
					ResourceSelectors: []policyv1alpha1.ResourceSelector{
						{
							APIVersion: deployment.APIVersion,
							Kind:       deployment.Kind,
							Name:       deployment.Name,
						},
					},
					Placement: policyv1alpha1.Placement{
						ClusterAffinity: &policyv1alpha1.ClusterAffinity{
							ClusterNames: framework.ClusterNames(),
						},
						SpreadConstraints: []policyv1alpha1.SpreadConstraint{
							{
								SpreadByField: policyv1alpha1.SpreadByFieldCluster,
								MaxGroups:     maxGroups,
								MinGroups:     minGroups,
							},
						},
					},
					PropagateDeps: true,
					Failover: &policyv1alpha1.FailoverBehavior{
						Application: &policyv1alpha1.ApplicationFailoverBehavior{
							DecisionConditions: policyv1alpha1.DecisionConditions{
								TolerationSeconds: ptr.To[int32](30),
							},
							PurgeMode: policyv1alpha1.Never,
						},
					},
				},
			}
		})

		ginkgo.BeforeEach(func() {
			framework.CreatePropagationPolicy(karmadaClient, policy)
			framework.CreateDeployment(kubeClient, deployment)
			ginkgo.DeferCleanup(func() {
				framework.RemoveDeployment(kubeClient, deployment.Namespace, deployment.Name)
				framework.RemovePropagationPolicy(karmadaClient, policy.Namespace, policy.Name)
			})
		})

		ginkgo.It("application failover with purgeMode never", func() {
			disabledClusters := framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
			ginkgo.By("create an error op", func() {
				overridePolicy = testhelper.NewOverridePolicyByOverrideRules(policyNamespace, policyName, []policyv1alpha1.ResourceSelector{
					{
						APIVersion: deployment.APIVersion,
						Kind:       deployment.Kind,
						Name:       deployment.Name,
					},
				}, []policyv1alpha1.RuleWithCluster{
					{
						TargetCluster: &policyv1alpha1.ClusterAffinity{
							ClusterNames: disabledClusters,
						},
						Overriders: policyv1alpha1.Overriders{
							ImageOverrider: []policyv1alpha1.ImageOverrider{
								{
									Component: "Registry",
									Operator:  policyv1alpha1.OverriderOpReplace,
									Value:     "fake",
								},
							},
						},
					},
				})
				framework.CreateOverridePolicy(karmadaClient, overridePolicy)
			})

			ginkgo.By("check if deployment present on member clusters has correct image value", func() {
				framework.WaitDeploymentPresentOnClustersFitWith(disabledClusters, deployment.Namespace, deployment.Name,
					func(deployment *appsv1.Deployment) bool {
						for _, container := range deployment.Spec.Template.Spec.Containers {
							if container.Image != "fake/nginx:1.19.0" {
								return false
							}
						}
						return true
					})
			})

			ginkgo.By("check whether the failed deployment is rescheduled to other available cluster", func() {
				gomega.Eventually(func() int {
					targetClusterNames := framework.ExtractTargetClustersFrom(controlPlaneClient, deployment)
					for _, targetClusterName := range targetClusterNames {
						// the target cluster should be overwritten to another available cluster
						if !testhelper.IsExclude(targetClusterName, disabledClusters) {
							return 0
						}
					}

					return len(targetClusterNames)
				}, pollTimeout, pollInterval).Should(gomega.Equal(minGroups))
			})

			ginkgo.By("check whether the failed deployment is present on the disabledClusters", func() {
				framework.WaitDeploymentPresentOnClustersFitWith(disabledClusters, deploymentNamespace, deploymentName, func(*appsv1.Deployment) bool { return true })
			})

			ginkgo.By("delete the error op", func() {
				framework.RemoveOverridePolicy(karmadaClient, policyNamespace, policyName)
			})
		})
	})
})

// disableCluster will set wrong API endpoint of current cluster
func disableCluster(c client.Client, clusterName string) error {
	err := wait.PollUntilContextTimeout(context.TODO(), pollInterval, pollTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterObj := &clusterv1alpha1.Cluster{}
		if err := c.Get(ctx, client.ObjectKey{Name: clusterName}, clusterObj); err != nil {
			return false, err
		}
		// set the APIEndpoint of matched cluster to a wrong value
		unavailableAPIEndpoint := "https://172.19.1.3:6443"
		clusterObj.Spec.APIEndpoint = unavailableAPIEndpoint
		if err := c.Update(ctx, clusterObj); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	return err
}

// taintCluster will taint cluster
func taintCluster(c client.Client, clusterName string, taint corev1.Taint) error {
	err := wait.PollUntilContextTimeout(context.TODO(), pollInterval, pollTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterObj := &clusterv1alpha1.Cluster{}
		if err := c.Get(ctx, client.ObjectKey{Name: clusterName}, clusterObj); err != nil {
			return false, err
		}
		clusterObj.Spec.Taints = append(clusterObj.Spec.Taints, taint)
		if err := c.Update(ctx, clusterObj); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	return err
}

// recoverTaintedCluster will recover the taint of the disabled cluster
func recoverTaintedCluster(c client.Client, clusterName string, taint corev1.Taint) error {
	err := wait.PollUntilContextTimeout(context.TODO(), pollInterval, pollTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterObj := &clusterv1alpha1.Cluster{}
		if err := c.Get(ctx, client.ObjectKey{Name: clusterName}, clusterObj); err != nil {
			return false, err
		}
		clusterObj.Spec.Taints = helper.SetCurrentClusterTaints(nil, []*corev1.Taint{&taint}, clusterObj)
		if err := c.Update(ctx, clusterObj); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	return err
}

// recoverCluster will recover API endpoint of the disable cluster
func recoverCluster(c client.Client, clusterName string, originalAPIEndpoint string) error {
	err := wait.PollUntilContextTimeout(context.TODO(), pollInterval, pollTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterObj := &clusterv1alpha1.Cluster{}
		if err := c.Get(ctx, client.ObjectKey{Name: clusterName}, clusterObj); err != nil {
			return false, err
		}
		clusterObj.Spec.APIEndpoint = originalAPIEndpoint
		if err := c.Update(ctx, clusterObj); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		fmt.Printf("recovered API endpoint is %s\n", clusterObj.Spec.APIEndpoint)
		return true, nil
	})
	return err
}

// get the API endpoint of a specific cluster
func getClusterAPIEndpoint(clusterName string) (apiEndpoint string) {
	for _, cluster := range framework.Clusters() {
		if cluster.Name == clusterName {
			apiEndpoint = cluster.Spec.APIEndpoint
			fmt.Printf("original API endpoint of the cluster %s is %s\n", clusterName, apiEndpoint)
		}
	}
	return apiEndpoint
}
