package webhookcerts

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	certapiv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	certv1informers "k8s.io/client-go/informers/certificates/v1"
	certv1client "k8s.io/client-go/kubernetes/typed/certificates/v1"
	certv1listers "k8s.io/client-go/listers/certificates/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

type CSRApprovalDecision string

const (
	CSRApproved  CSRApprovalDecision = "Approved"
	CSRDenied    CSRApprovalDecision = "Denied"
	CSRNoOpinion CSRApprovalDecision = "NoOpinion"
)

type CSRApprover interface {
	Approve(csrObj *certapiv1.CertificateSigningRequest, x509CSR *x509.CertificateRequest) (approvalStatus CSRApprovalDecision, denyReason string, err error)
}

type csrApproverController struct {
	csrClient certv1client.CertificateSigningRequestInterface
	csrLister certv1listers.CertificateSigningRequestLister

	csrName     string
	csrApprover CSRApprover
}

// NewCSRApproverController returns a controller that is observing the CSR API
// for a CSR of a given name. If such a CSR exists, it runs the `csrApprover.Approve()`
// against it and either denies, approves or leaves the CSR.
func NewCSRApproverController(
	controllerName string,
	operatorClient v1helpers.OperatorClient,
	csrClient certv1client.CertificateSigningRequestInterface,
	csrInformers certv1informers.CertificateSigningRequestInformer,
	csrName string,
	csrApprover CSRApprover,
	eventsRecorder events.Recorder,
) factory.Controller {
	c := &csrApproverController{
		csrClient:   csrClient,
		csrLister:   csrInformers.Lister(),
		csrName:     csrName,
		csrApprover: csrApprover,
	}

	return factory.New().
		WithSync(c.sync).
		WithSyncDegradedOnError(operatorClient).
		WithFilteredEventsInformers(factory.NamesFilter(c.csrName), csrInformers.Informer()).
		ToController(
			"WebhookAuthenticatorCertApprover_"+controllerName,
			eventsRecorder.WithComponentSuffix("webhook-authenticator-cert-approver-"+controllerName),
		)
}

func (c *csrApproverController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	csr, err := c.csrLister.Get(c.csrName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if approved, denied := getCertApprovalCondition(&csr.Status); approved || denied {
		return nil
	}

	csrCopy := csr.DeepCopy()

	csrPEM, _ := pem.Decode(csr.Spec.Request)
	if csrPEM == nil {
		return fmt.Errorf("failed to PEM-parse the CSR block in .spec.request: no CSRs were found")
	}

	x509CSR, err := x509.ParseCertificateRequest(csrPEM.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse the CSR bytes: %v", err)
	}

	if x509CSR.Subject.CommonName == csr.Spec.Username {
		c.denyCSR(ctx, csrCopy, "IllegitimateRequester", "requester cannot request certificates for themselves", syncCtx.Recorder())
	}

	csrDecision, denyReason, err := c.csrApprover.Approve(csr, x509CSR)
	if err != nil {
		return c.denyCSR(ctx, csrCopy, "CSRApprovingFailed", fmt.Sprintf("there was an error during CSR approval: %v", err), syncCtx.Recorder())
	}

	switch csrDecision {
	case CSRDenied:
		return c.denyCSR(ctx, csrCopy, "CSRDenied", denyReason, syncCtx.Recorder())
	case CSRApproved:
		return c.approveCSR(ctx, csrCopy, syncCtx.Recorder())
	case CSRNoOpinion:
		fallthrough
	default:
		return nil
	}
}

func (c *csrApproverController) denyCSR(ctx context.Context, csrCopy *certapiv1.CertificateSigningRequest, reason, message string, eventsRecorder events.Recorder) error {
	csrCopy.Status.Conditions = append(csrCopy.Status.Conditions,
		certapiv1.CertificateSigningRequestCondition{
			Type:    certapiv1.CertificateDenied,
			Status:  corev1.ConditionTrue,
			Reason:  reason,
			Message: message,
		},
	)

	eventsRecorder.Eventf("CSRDenial", "The CSR %q has been denied: %s - %s", csrCopy.Name, reason, message)
	_, err := c.csrClient.UpdateApproval(ctx, csrCopy.Name, csrCopy, v1.UpdateOptions{})
	return err
}

func (c *csrApproverController) approveCSR(ctx context.Context, csrCopy *certapiv1.CertificateSigningRequest, eventsRecorder events.Recorder) error {
	csrCopy.Status.Conditions = append(csrCopy.Status.Conditions,
		certapiv1.CertificateSigningRequestCondition{
			Type:    certapiv1.CertificateApproved,
			Status:  corev1.ConditionTrue,
			Reason:  "AutoApproved",
			Message: fmt.Sprintf("Auto-approved CSR %q", csrCopy.Name),
		})

	eventsRecorder.Eventf("CSRApproval", "The CSR %q has been approved", csrCopy.Name)
	_, err := c.csrClient.UpdateApproval(ctx, csrCopy.Name, csrCopy, v1.UpdateOptions{})
	return err
}

func getCertApprovalCondition(status *certapiv1.CertificateSigningRequestStatus) (approved bool, denied bool) {
	for _, c := range status.Conditions {
		if c.Type == certapiv1.CertificateApproved {
			approved = true
		}
		if c.Type == certapiv1.CertificateDenied {
			denied = true
		}
	}
	return
}

type ServiceAccountApprover struct {
	saGroups        sets.String // saGroups is the set of groups for the SA expected to have created the CSR
	saName          string
	expectedSubject string
}

// ServiceAccountApprover approves CSRs with a given subject issued by the provided service account
func NewServiceAccountApprover(saNamespace, saName, expectedSubject string, additionalGroups ...string) *ServiceAccountApprover {
	saGroups := append(serviceaccount.MakeGroupNames(saNamespace), "system:authenticated")

	return &ServiceAccountApprover{
		saName:          serviceaccount.MakeUsername(saNamespace, saName),
		saGroups:        sets.NewString(append(saGroups, additionalGroups...)...),
		expectedSubject: expectedSubject,
	}
}

func (a *ServiceAccountApprover) Approve(csrObj *certapiv1.CertificateSigningRequest, x509CSR *x509.CertificateRequest) (approvalStatus CSRApprovalDecision, denyReason string, err error) {
	if csrObj == nil || x509CSR == nil {
		return CSRDenied, "Error", fmt.Errorf("received a 'nil' CSR")
	}

	if csrObj.Spec.Username != a.saName {
		return CSRDenied, fmt.Sprintf("CSR %q was created by an unexpected user: %q", csrObj.Name, csrObj.Spec.Username), nil
	}

	if csrGroups := sets.NewString(csrObj.Spec.Groups...); !csrGroups.Equal(a.saGroups) {
		return CSRDenied, fmt.Sprintf("CSR %q was created by a user with unexpected groups: %v", csrObj.Name, csrGroups.List()), nil
	}

	if expectedSubject := a.expectedSubject; x509CSR.Subject.String() != expectedSubject {
		return CSRDenied, fmt.Sprintf("expected the CSR's subject to be %q, but it is %q", expectedSubject, x509CSR.Subject.String()), nil
	}

	return CSRApproved, "", nil

}
