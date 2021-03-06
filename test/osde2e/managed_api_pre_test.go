package osde2e

import (
	goctx "context"
	"fmt"
	integreatlyv1alpha1 "github.com/integr8ly/integreatly-operator/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/integreatly-operator/test/common"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	dynclient "sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

var (
	pagerDutySecretName = common.NamespacePrefix + "pagerduty"
	deadMansSnitchName  = common.NamespacePrefix + "deadmanssnitch"
	smtpSecretName      = common.NamespacePrefix + "smtp"
)

// This tests if an installation of Managed-API was finished and is successful
func PreTest(t *testing.T, ctx *common.TestingContext) {
	err := wait.Poll(time.Second*15, time.Minute*40, func() (done bool, err error) {

		rhmi, err := getRHMI(ctx.Client)
		if err != nil {
			t.Fatalf("error getting RHMI CR: %v", err)
		}

		// Patch Managed-API CR with cluster storage
		if rhmi.Spec.UseClusterStorage == "" {
			rhmiCR := fmt.Sprintf(`{
				"apiVersion": "integreatly.org/v1alpha1",
				"kind": "RHMI",
				"spec": {
					"useClusterStorage" : "true"
				}
			}`)

			rhmiCRBytes := []byte(rhmiCR)

			request := ctx.ExtensionClient.RESTClient().Patch(types.MergePatchType).
				Resource("rhmis").
				Name("managed-api").
				Namespace("redhat-rhmi-operator").
				RequestURI("/apis/integreatly.org/v1alpha1").Body(rhmiCRBytes).Do()
			_, err := request.Raw()

			if err != nil {
				return false, err
			}
		}

		// Get smtp secret - if failed - create SMTP Secret
		_, err = getSecret(ctx.Client, smtpSecretName)
		if err != nil {
			smtpSec := &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprint(smtpSecretName),
					Namespace: common.RHMIOperatorNamespace,
				},
				Data: map[string][]byte{
					"host":     []byte("test"),
					"password": []byte("test"),
					"port":     []byte("test"),
					"tls":      []byte("test"),
					"username": []byte("test"),
				},
			}
			if err := ctx.Client.Create(goctx.TODO(), smtpSec.DeepCopy()); err != nil {
				t.Fatalf("Failed to create Pager Duty Secret: %v", err)
			}
		}

		// Get pagerduty secret - if failed - create
		_, err = getSecret(ctx.Client, pagerDutySecretName)
		if err != nil {

			pagerDuty := v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pagerDutySecretName,
					Namespace: common.RHMIOperatorNamespace,
				},
				Data: map[string][]byte{
					"serviceKey": []byte("test"),
				},
			}
			if err := ctx.Client.Create(goctx.TODO(), pagerDuty.DeepCopy()); err != nil {
				t.Fatalf("Failed to create Pager Duty Secret: %v", err)
			}
		}

		// Get dms - if failed - create
		_, err = getSecret(ctx.Client, deadMansSnitchName)
		if err != nil {

			dms := v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deadMansSnitchName,
					Namespace: common.RHMIOperatorNamespace,
				},
				Data: map[string][]byte{
					"url": []byte("test"),
				},
			}
			if err := ctx.Client.Create(goctx.TODO(), dms.DeepCopy()); err != nil {
				t.Fatalf("Failed to create DMS secret: %v", err)
			}
		}

		if rhmi.Status.Stage != "complete" {
			t.Logf("Current stage is %v", rhmi.Status.Stage)
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		t.Errorf("Something went wrong ...%v", err)
	}
}

func getRHMI(client dynclient.Client) (*integreatlyv1alpha1.RHMI, error) {
	rhmi := &integreatlyv1alpha1.RHMI{}
	if err := client.Get(goctx.TODO(), types.NamespacedName{Name: InstallationName, Namespace: common.RHMIOperatorNamespace}, rhmi); err != nil {
		return nil, fmt.Errorf("error getting RHMI CR: %w", err)
	}
	return rhmi, nil
}

func getSecret(client dynclient.Client, secretName string) (*v1.Secret, error) {
	secret := &v1.Secret{}

	if err := client.Get(goctx.TODO(), types.NamespacedName{Name: secretName, Namespace: common.RHMIOperatorNamespace}, secret); err != nil {
		return nil, fmt.Errorf("Error getting secret")
	}
	return secret, nil
}
