package suites

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	fluxhelmv2 "github.com/fluxcd/helm-controller/api/v2"
	apimeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nutanix-cloud-native/nkp-partner-catalog/apptests/catalog"
)

const (
	operantRegistryServerEnvVar = "OPERANT_REGISTRY"
	operantKeyIDEnvVar          = "OPERANT_KEY_ID"
	operantKeySecretEnvVar      = "OPERANT_KEY_SECRET" //nolint:gosec // not a credential, just an env var name
	registryAuthSecretSuffix    = "registry-auth"
	configDefaultsSuffix        = "config-defaults"
	operantRegistrySecretName   = "operant-registry-creds"
	operantGatekeeperName       = "Operant_Nutanix"
	operantTokenEnvVar          = "OPERANT_TOKEN" //nolint:gosec // not a credential, just an env var name
	operantNamespace            = "operant-system"
)

// resolverSuffix returns the name a "${releaseName}-<suffix>" resource resolves
// to in the apptests environment. catalog.App.install substitutes only
// ${releaseName} and ${releaseNamespace}; ${appVersion} resolves to an empty
// string (see apptests/catalog/app.go), hence the double dash.
func resolverSuffix(releaseName, suffix string) string {
	return fmt.Sprintf("%s--%s", releaseName, suffix)
}

func getOperantRegistry() string {
	registry, ok := os.LookupEnv(operantRegistryServerEnvVar)
	if !ok {
		return "registry.operant.ai"
	} else {
		return registry
	}
}

// createOperantRegistrySecret returns a kubernetes.io/dockerconfigjson Secret.
func createOperantRegistrySecret(name, namespace string) *unstructured.Unstructured {
	username := os.Getenv(operantKeyIDEnvVar)
	password := os.Getenv(operantKeySecretEnvVar)

	dockerConfigJSON, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			getOperantRegistry(): map[string]any{
				"username": username,
				"password": password,
				"auth":     base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
			},
		},
	},
	)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal dockerconfigjson: %v", err))
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"type": "kubernetes.io/dockerconfigjson",
			"data": map[string]any{
				".dockerconfigjson": base64.StdEncoding.EncodeToString(dockerConfigJSON),
			},
		},
	}
}

// setOperantLCPValues patches the defaults ConfigMap so the chart pulls images
// using the same registry credentials used for the Helm chart pull.
func setOperantLCPValues(ctx context.Context, releaseName, namespace string) error {
	cm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      resolverSuffix(releaseName, configDefaultsSuffix),
				"namespace": namespace,
			},
		},
	}

	// The configmap struct object will be updated with the object already present in the cluster
	if err := k8sClient.Get(
		ctx,
		ctrlClient.ObjectKey{Name: resolverSuffix(releaseName, configDefaultsSuffix), Namespace: namespace},
		cm,
	); err != nil {
		return err
	}

	valuesYAML, _, _ := unstructured.NestedString(cm.Object, "data", "values.yaml")
	// setting required values for CM
	valuesYAML += fmt.Sprintf(`
env:
  GATEKEEPER_NAME: %q
  OPERANT_TOKEN: %q

operantRegistry:
  createSecret: true
  username: %q
  password: %q
  server: %q
`, operantGatekeeperName,
		os.Getenv(operantTokenEnvVar),
		os.Getenv(operantKeyIDEnvVar),
		os.Getenv(operantKeySecretEnvVar),
		getOperantRegistry())

	if err := unstructured.SetNestedField(cm.Object, valuesYAML, "data", "values.yaml"); err != nil {
		return err
	}
	return k8sClient.Update(ctx, cm)
}

func operantRegistryAuthSecretName(releaseName string) string {
	return resolverSuffix(releaseName, registryAuthSecretSuffix)
}

func operantCredentialsPresent() bool {
	return os.Getenv(operantKeyIDEnvVar) != "" && os.Getenv(operantKeySecretEnvVar) != ""
}

var _ = Describe("operant-lcp Tests", Label("operant-lcp"), func() {
	Describe("Installing operant-lcp", Ordered, Label("install"), func() {
		var (
			c  *catalog.App
			hr *fluxhelmv2.HelmRelease
		)

		BeforeAll(func() {
			if !operantCredentialsPresent() {
				Skip(
					fmt.Sprintf(
						"skipping operant-lcp test: %s and %s env vars not set",
						operantKeyIDEnvVar,
						operantKeySecretEnvVar,
					),
				)
			}

			err := SetupKindCluster()
			Expect(err).ToNot(HaveOccurred())

			err = env.InstallLatestFlux(ctx)
			Expect(err).ToNot(HaveOccurred())
		})

		AfterAll(func() {
			if useExistingCluster || os.Getenv("SKIP_CLUSTER_TEARDOWN") != "" {
				return
			}

			err := env.Destroy(ctx)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should install successfully with default config", func() {
			c = catalog.NewAppScenario("operant-lcp", *appVersion).(*catalog.App)

			By("creating the registry auth secret for the chart pull")
			registrySecret := createOperantRegistrySecret(
				operantRegistryAuthSecretName(c.Name()),
				catalog.DefaultNamespace,
			)
			err := k8sClient.Create(ctx, registrySecret)
			Expect(err).ToNot(HaveOccurred())

			By("installing the HelmRelease")
			GinkgoWriter.Printf("Installing %s @ %s\n", c.Name(), *appVersion)
			err = c.Install(ctx, env)
			Expect(err).ToNot(HaveOccurred())

			By("injecting registry credentials into the values ConfigMap")
			err = setOperantLCPValues(ctx, c.Name(), catalog.DefaultNamespace)
			Expect(err).ToNot(HaveOccurred())
			GinkgoWriter.Printf("Install applied, waiting for HelmRelease to become Ready\n")

			hr = &fluxhelmv2.HelmRelease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      c.Name(),
					Namespace: catalog.DefaultNamespace,
				},
			}

			Eventually(func() error {
				err = k8sClient.Get(ctx, ctrlClient.ObjectKeyFromObject(hr), hr)
				if err != nil {
					GinkgoWriter.Printf("HelmRelease Get error: %v\n", err)
					return err
				}

				GinkgoWriter.Printf("HelmRelease %s/%s conditions: %v\n",
					hr.Namespace, hr.Name, hr.Status.Conditions)

				for _, cond := range hr.Status.Conditions {
					if cond.Status == metav1.ConditionTrue &&
						cond.Type == apimeta.ReadyCondition {
						GinkgoWriter.Printf("HelmRelease is Ready!\n")
						return nil
					}
				}
				return fmt.Errorf("helm release not ready yet")
			}).WithPolling(catalog.PollInterval).WithTimeout(10 * time.Minute).Should(Succeed())
		})
	})

	Describe("Upgrading operant-lcp", Ordered, Label("upgrade"), func() {
		var (
			c  *catalog.App
			hr *fluxhelmv2.HelmRelease
		)

		BeforeAll(func() {
			if !operantCredentialsPresent() {
				Skip(
					fmt.Sprintf(
						"skipping operant-lcp upgrade test: %s and %s env vars not set",
						operantKeyIDEnvVar,
						operantKeySecretEnvVar,
					),
				)
			}

			c = catalog.NewAppScenario("operant-lcp", *appVersion).(*catalog.App)
			if !c.HasPreviousVersion() {
				Skip("skipping upgrade test: no previous version available")
			}

			err := SetupKindCluster()
			Expect(err).ToNot(HaveOccurred())

			err = env.InstallLatestFlux(ctx)
			Expect(err).ToNot(HaveOccurred())
		})

		AfterAll(func() {
			if useExistingCluster || os.Getenv("SKIP_CLUSTER_TEARDOWN") != "" {
				return
			}

			err := env.Destroy(ctx)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should install the previous version successfully", func() {
			registrySecret := createOperantRegistrySecret(
				operantRegistryAuthSecretName(c.Name()),
				catalog.DefaultNamespace,
			)
			err := k8sClient.Create(ctx, registrySecret)
			Expect(err).ToNot(HaveOccurred())

			err = c.InstallPreviousVersion(ctx, env)
			Expect(err).ToNot(HaveOccurred())

			err = setOperantLCPValues(ctx, c.Name(), catalog.DefaultNamespace)
			Expect(err).ToNot(HaveOccurred())

			hr = &fluxhelmv2.HelmRelease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      c.Name(),
					Namespace: catalog.DefaultNamespace,
				},
			}

			Eventually(func() error {
				err = k8sClient.Get(ctx, ctrlClient.ObjectKeyFromObject(hr), hr)
				if err != nil {
					return err
				}

				for _, cond := range hr.Status.Conditions {
					if cond.Status == metav1.ConditionTrue &&
						cond.Type == apimeta.ReadyCondition {
						return nil
					}
				}
				return fmt.Errorf("helm release not ready yet")
			}).WithPolling(catalog.PollInterval).WithTimeout(10 * time.Minute).Should(Succeed())
		})

		It("should upgrade operant-lcp successfully", func() {
			err := c.Upgrade(ctx, env)
			Expect(err).ToNot(HaveOccurred())

			hr = &fluxhelmv2.HelmRelease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      c.Name(),
					Namespace: catalog.DefaultNamespace,
				},
			}

			Eventually(func() error {
				err = k8sClient.Get(ctx, ctrlClient.ObjectKeyFromObject(hr), hr)
				if err != nil {
					return err
				}

				for _, cond := range hr.Status.Conditions {
					if cond.Status == metav1.ConditionTrue &&
						cond.Type == apimeta.ReadyCondition &&
						cond.Reason == fluxhelmv2.UpgradeSucceededReason {
						return nil
					}
				}
				return fmt.Errorf("helm release not ready yet")
			}).WithPolling(catalog.PollInterval).WithTimeout(10 * time.Minute).Should(Succeed())
		})
	})
})
