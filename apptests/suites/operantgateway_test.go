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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nutanix-cloud-native/nkp-partner-catalog/apptests/catalog"
)

const (
	operantGatewayConfigSecretName = "operant-gateway"
	operantLCPName                 = "operant-lcp"
)

// createOperantGatewayConfigSecret returns the Secret the gateway chart consumes with the Gateway config file.
func createOperantGatewayConfigSecret(name, namespace string) *unstructured.Unstructured {
	operantToken := os.Getenv(operantTokenEnvVar)
	gatekeeperURL := fmt.Sprintf("http://%s.%s.svc:80", operantLCPName, namespace)

	gatewayConfigJSON, err := json.Marshal(map[string]any{
		"gateway": map[string]any{
			"user_email":          "user@example.com",
			"operant_token":       operantToken,
			"team_name":           "your-team",
			"use_streamable_http": true,
			"gatekeeper_url":      gatekeeperURL,
			"service_name":        "",
		},
	},
	)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal Gateway config: %v", err))
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"type": "Opaque",
			"data": map[string]any{
				"operant_mcp_gateway.json": base64.StdEncoding.EncodeToString(gatewayConfigJSON),
			},
		},
	}
}

// installingOperantLCP installs operant-lcp in the test cluster so that
// operant-gateway has its dependency available, mirroring how the flux
// `dependsOn` would behave in a real NKP deployment (which bypasses the
// catalog.App install path). It reuses the same steps as the operant-lcp suite:
// the app manifests under applications/operant-lcp/<version>/helmrelease contain
// the OCI dependency objects (OCIRepository + HelmRelease + values ConfigMap).
func installingOperantLCP() error {
	GinkgoHelper()

	lcp := catalog.NewAppScenario(operantLCPName, *appVersion).(*catalog.App)

	By("creating the operant-lcp registry auth secret for the chart pull")
	registrySecret := createOperantRegistrySecret(registryAuthName,
		catalog.DefaultNamespace)
	if err := k8sClient.Create(ctx, registrySecret); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	By("installing the operant-lcp HelmRelease from the app manifests")
	if err := lcp.Install(ctx, env); err != nil {
		return err
	}

	By("injecting the operant-lcp values into the defaults ConfigMap")
	if err := setOperantLCPValues(ctx, lcp.Name(), catalog.DefaultNamespace); err != nil {
		return err
	}

	hr := &fluxhelmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lcp.Name(),
			Namespace: catalog.DefaultNamespace,
		},
	}

	Eventually(func() error {
		if err := k8sClient.Get(ctx, ctrlClient.ObjectKeyFromObject(hr), hr); err != nil {
			GinkgoWriter.Printf("operant-lcp HelmRelease Get error: %v\n", err)
			return err
		}

		GinkgoWriter.Printf("HelmRelease %s/%s conditions: %v\n",
			hr.Namespace, hr.Name, hr.Status.Conditions)

		for _, cond := range hr.Status.Conditions {
			if cond.Status == metav1.ConditionTrue &&
				cond.Type == apimeta.ReadyCondition {
				GinkgoWriter.Printf("operant-lcp HelmRelease is Ready!\n")
				return nil
			}
		}
		return fmt.Errorf("operant-lcp helm release not ready yet")
	}).WithPolling(catalog.PollInterval).WithTimeout(10 * time.Minute).Should(Succeed())

	return nil
}

// setOperantGatewayValues patches the defaults ConfigMap with the registry creds.
func setOperantGatewayValues(ctx context.Context, releaseName, namespace string) error {
	cm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
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
	// Setting custom vlaues.yaml for the CM
	valuesYAML += fmt.Sprintf(`
useConfigSecret: true
operantRegistry:
  createSecret: true
  username: %q
  password: %q
  server: %q
`, os.Getenv(operantKeyIDEnvVar), os.Getenv(operantKeySecretEnvVar), getOperantRegistry())

	if err := unstructured.SetNestedField(cm.Object, valuesYAML, "data", "values.yaml"); err != nil {
		return err
	}
	return k8sClient.Update(ctx, cm)
}

var _ = Describe("operant-gateway Tests", Label("operant-gateway"), func() {
	Describe("Installing operant-gateway", Ordered, Label("install"), func() {
		var (
			c  *catalog.App
			hr *fluxhelmv2.HelmRelease
		)

		BeforeAll(func() {
			if !operantCredentialsPresent() {
				Skip(
					fmt.Sprintf(
						"skipping operant-gateway test: %s and %s env vars not set",
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
			c = catalog.NewAppScenario("operant-gateway", *appVersion).(*catalog.App)

			By("installing the operant-lcp prerequisite")
			Expect(installingOperantLCP()).To(Succeed())

			By("creating the gateway config secret")
			configSecret := createOperantGatewayConfigSecret(operantGatewayConfigSecretName, catalog.DefaultNamespace)
			err := k8sClient.Create(ctx, configSecret)
			Expect(err).ToNot(HaveOccurred())

			By("installing the HelmRelease")
			GinkgoWriter.Printf("Installing %s @ %s\n", c.Name(), *appVersion)
			err = c.Install(ctx, env)
			Expect(err).ToNot(HaveOccurred())

			By("injecting registry credentials into the values ConfigMap")
			err = setOperantGatewayValues(ctx, c.Name(), catalog.DefaultNamespace)
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
})
