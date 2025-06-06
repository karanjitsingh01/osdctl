package resize

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	bpelevate "github.com/openshift/backplane-cli/pkg/elevate"
	"github.com/openshift/osdctl/cmd/servicelog"
	"github.com/openshift/osdctl/pkg/k8s"
	"github.com/openshift/osdctl/pkg/printer"
	"github.com/openshift/osdctl/pkg/utils"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	resizeControlPlaneServiceLogTemplate = "https://raw.githubusercontent.com/openshift/managed-notifications/master/osd/controlplane_resized.json"
	cpmsNamespace                        = "openshift-machine-api"
	cpmsName                             = "cluster"
)

// controlPlane defines the struct for running resizeControlPlaneNode command
type controlPlane struct {
	clusterID      string
	newMachineType string
	cluster        *cmv1.Cluster

	// client is a K8s client to cluster
	client client.Client

	// clientAdmin is a K8s client to cluster impersonating backplane-cluster-admin
	clientAdmin client.Client

	// reason to provide for elevation (eg: OHSS/PG ticket)
	reason string
}

// This command requires to previously be logged in via `ocm login`
func newCmdResizeControlPlane() *cobra.Command {
	ops := &controlPlane{}
	resizeControlPlaneNodeCmd := &cobra.Command{
		Use:   "control-plane",
		Short: "Resize an OSD/ROSA cluster's control plane nodes",
		Long: `Resize an OSD/ROSA cluster's control plane nodes

  Requires previous login to the api server via "ocm backplane login".
  The user will be prompted to send a service log after initiating the resize. The resize process runs asynchronously,
  and this command exits immediately after sending the service log. Any issues with the resize will be reported via PagerDuty.`,
		Example: `
  # Resize all control plane instances to m5.4xlarge using control plane machine sets
  osdctl cluster resize control-plane -c "${CLUSTER_ID}" --machine-type m5.4xlarge --reason "${OHSS}"`,
		Args:              cobra.NoArgs,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ops.New(); err != nil {
				return err
			}
			return ops.run(context.Background())
		},
	}
	resizeControlPlaneNodeCmd.Flags().StringVarP(&ops.clusterID, "cluster-id", "c", "", "The internal ID of the cluster to perform actions on")
	resizeControlPlaneNodeCmd.Flags().StringVar(&ops.newMachineType, "machine-type", "", "The target AWS machine type to resize to (e.g. m5.2xlarge)")
	resizeControlPlaneNodeCmd.Flags().StringVar(&ops.reason, "reason", "", "The reason for this command, which requires elevation, to be run (usually an OHSS or PD ticket)")
	_ = resizeControlPlaneNodeCmd.MarkFlagRequired("cluster-id")
	_ = resizeControlPlaneNodeCmd.MarkFlagRequired("machine-type")
	_ = resizeControlPlaneNodeCmd.MarkFlagRequired("reason")

	return resizeControlPlaneNodeCmd
}

func (o *controlPlane) New() error {
	if err := validateInstanceSize(o.newMachineType, "controlplane"); err != nil {
		return err
	}

	if o.cluster != nil && o.cluster.Hypershift().Enabled() {
		return errors.New("this command should not be used for HCP clusters")
	}

	err := utils.IsValidClusterKey(o.clusterID)
	if err != nil {
		return err
	}

	connection, err := utils.CreateConnection()
	if err != nil {
		return err
	}
	defer connection.Close()

	cluster, err := utils.GetCluster(connection, o.clusterID)
	if err != nil {
		return err
	}

	o.cluster = cluster

	// Ensure we store the internal OCM cluster id
	o.clusterID = cluster.ID()

	scheme := runtime.NewScheme()
	// Register machinev1 for ControlPlaneMachineSets
	if err := machinev1.Install(scheme); err != nil {
		return err
	}

	c, err := k8s.New(o.clusterID, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	cAdmin, err := k8s.NewAsBackplaneClusterAdmin(o.cluster.ID(), client.Options{Scheme: scheme}, []string{
		o.reason,
		fmt.Sprintf("Need elevation for %s cluster in order to resize it to instance type %s", o.clusterID, o.newMachineType),
	}...)
	if err != nil {
		return err
	}

	o.client = c
	o.clientAdmin = cAdmin
	return nil
}

func (o *controlPlane) embiggenMachineType() {}

type optionsDialogResponse int64

const (
	Undefined optionsDialogResponse = 0
	Retry                           = 1
	Skip                            = 2
	Force                           = 3
	Cancel                          = 4
)

func retryCancelDialog(procedure string) (optionsDialogResponse, error) {
	fmt.Printf("Do you want to retry %s or cancel this command? (retry/cancel):\n", procedure)

	reader := bufio.NewReader(os.Stdin)

	responseBytes, _, err := reader.ReadLine()
	if err != nil {
		return Undefined, fmt.Errorf("reader.ReadLine() resulted in an error: %s", err)
	}

	response := strings.ToUpper(string(responseBytes))

	switch response {
	case "RETRY":
		return Retry, nil
	case "CANCEL":
		return Cancel, nil
	default:
		fmt.Println("Invalid response, expected 'retry' or 'cancel' (case-insensitive).")
		return retryCancelDialog(procedure)
	}
}

func withRetryCancelOption(fn func() error, procedure string) (err error) {
	err = fn()
	if err == nil {
		return nil
	}
	dialogResponse, err := retryCancelDialog(procedure)
	if err != nil {
		return err
	}

	switch dialogResponse {
	case Retry:
		return withRetryCancelOption(fn, procedure)
	case Cancel:
		return errors.New("exiting")
	default:
		return errors.New("unhandled enumerator in withRetryCancelOption")
	}
}

func retrySkipCancelDialog(procedure string) (optionsDialogResponse, error) {
	fmt.Printf("Do you want to retry %[1]s, skip %[1]s or cancel this command? (retry/skip/cancel):\n", procedure)

	reader := bufio.NewReader(os.Stdin)

	responseBytes, _, err := reader.ReadLine()
	if err != nil {
		return Undefined, fmt.Errorf("reader.ReadLine() resulted in an error: %s", err)
	}

	response := strings.ToUpper(string(responseBytes))

	switch response {
	case "RETRY":
		return Retry, nil
	case "SKIP":
		return Skip, nil
	case "CANCEL":
		return Cancel, nil
	default:
		fmt.Println("Invalid response, expected 'retry', 'skip' or 'cancel' (case-insensitive).")
		return retrySkipCancelDialog(procedure)
	}
}

func withRetrySkipCancelOption(fn func() error, procedure string) (err error) {
	err = fn()
	if err == nil {
		return nil
	}
	fmt.Println(err)
	dialogResponse, err := retrySkipCancelDialog(procedure)
	if err != nil {
		return err
	}

	switch dialogResponse {
	case Retry:
		return withRetrySkipCancelOption(fn, procedure)
	case Skip:
		fmt.Printf("Skipping %s...\n", procedure)
	case Cancel:
		return errors.New("exiting")
	default:
		return errors.New("unhandled enumerator in withRetrySkipCancelOption")
	}
	return nil
}

func retrySkipForceCancelDialog(procedure string) (optionsDialogResponse, error) {
	fmt.Printf("Do you want to retry %s, skip %s, force %s or cancel this command? (retry/skip/force/cancel):\n", procedure, procedure, procedure)

	reader := bufio.NewReader(os.Stdin)

	responseBytes, _, err := reader.ReadLine()
	if err != nil {
		return Undefined, fmt.Errorf("reader.ReadLine() resulted in an error: %s", err)
	}

	response := strings.ToUpper(string(responseBytes))

	switch response {
	case "RETRY":
		return Retry, nil
	case "SKIP":
		return Skip, nil
	case "FORCE":
		return Force, nil
	case "CANCEL":
		return Cancel, nil
	default:
		fmt.Println("Invalid response, expected 'retry', 'skip', 'force' or 'cancel' (case-insensitive).")
		return retrySkipForceCancelDialog(procedure)
	}
}

func (o *controlPlane) forceDrainNode(nodeID string, reason string) error {
	printer.PrintlnGreen("Force draining node... This might take a minute or two...")
	err := bpelevate.RunElevate([]string{
		fmt.Sprintf("%s - Elevate required to force drain node for resizecontroleplanenode", reason),
		"adm drain --ignore-daemonsets --delete-emptydir-data --force", nodeID,
	})
	if err != nil {
		return fmt.Errorf("failed to force drain:\n%s", err)
	}
	return nil
}

func (o *controlPlane) drainNode(nodeID string, reason string) error {
	printer.PrintlnGreen("Draining node", nodeID)

	err := bpelevate.RunElevate([]string{
		fmt.Sprintf("%s - Elevate required to drain node for resizecontroleplanenode", reason),
		"adm drain --ignore-daemonsets --delete-emptydir-data", nodeID,
	})
	if err != nil {
		fmt.Println("Failed to drain node:")
		fmt.Println(err)

		dialogResponse, err := retrySkipForceCancelDialog("draining node")
		if err != nil {
			return err
		}

		switch dialogResponse {
		case Retry:
			return o.drainNode(nodeID, reason)
		case Skip:
			fmt.Println("Skipping node drain")
		case Force:
			err = withRetrySkipCancelOption(func() error { return o.forceDrainNode(nodeID, reason) }, "force draining")
			if err != nil {
				return err
			}
		case Cancel:
			return errors.New("exiting")
		}
	}
	return nil
}

func stopNode(ctx context.Context, awsClient resizeControlPlaneNodeAWSClient, nodeID string) error {
	printer.PrintfGreen("Stopping ec2 instance %s. This might take a minute or two...\n", nodeID)

	_, err := awsClient.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{nodeID},
	})
	if err != nil {
		return fmt.Errorf("unable to request stop of ec2 instance: %v", err)
	}

	waiter := ec2.NewInstanceStoppedWaiter(awsClient)
	describeInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: []string{nodeID},
	}

	err = waiter.Wait(ctx, describeInstancesInput, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("unable to stop or timed out while stopping ec2 instance: %s", err)
	}
	return nil
}

func modifyInstanceAttribute(ctx context.Context, awsClient resizeControlPlaneNodeAWSClient, nodeID string, newMachineType string) error {
	printer.PrintlnGreen("Modifying machine type of instance:", nodeID, "to", newMachineType)

	modifyInstanceAttributeInput := &ec2.ModifyInstanceAttributeInput{InstanceId: &nodeID, InstanceType: &types.AttributeValue{Value: &newMachineType}}

	_, err := awsClient.ModifyInstanceAttribute(ctx, modifyInstanceAttributeInput)
	if err != nil {
		return fmt.Errorf("unable to modify ec2 instance: %v", err)
	}
	return nil
}

func startNode(ctx context.Context, awsClient resizeControlPlaneNodeAWSClient, nodeID string) error {
	printer.PrintfGreen("Starting instance %s. This might take a minute or two...\n", nodeID)

	_, err := awsClient.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{nodeID},
	})
	if err != nil {
		return fmt.Errorf("unable to request start of ec2 instance: %v", err)
	}

	waiter := ec2.NewInstanceRunningWaiter(awsClient)
	describeInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: []string{nodeID},
	}

	err = waiter.Wait(ctx, describeInstancesInput, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("unable to run or timed out while running ec2 instance: %s", err)
	}
	return nil
}

func uncordonNode(nodeID string) error {
	printer.PrintlnGreen("Uncordoning node", nodeID)
	cmd := fmt.Sprintf("oc adm uncordon %s", nodeID)
	output, err := exec.Command("bash", "-c", cmd).CombinedOutput()

	if err != nil {
		fmt.Printf("Failed to uncordon node: %s", strings.TrimSpace(string(output)))
		return err
	}
	return nil
}

func getNodeAwsInstanceData(ctx context.Context, node string, awsClient resizeControlPlaneNodeAWSClient) (string, string, error) {
	params := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("private-dns-name"),
				Values: []string{node},
			},
		},
	}
	ret, err := awsClient.DescribeInstances(ctx, params)
	if err != nil {
		return "", "", err
	}

	if len(ret.Reservations) == 0 || len(ret.Reservations[0].Instances) == 0 {
		return "", "", errors.New("no instances found for the given node")
	}

	awsInstanceID := *ret.Reservations[0].Instances[0].InstanceId

	var machineName string
	tags := ret.Reservations[0].Instances[0].Tags
	for _, t := range tags {
		if *t.Key == "Name" {
			machineName = *t.Value
			break
		}
	}

	if machineName == "" {
		return "", "", errors.New("could not retrieve node machine name")
	}

	fmt.Println("Node", node, "found as AWS internal InstanceId", awsInstanceID, "with machine name", machineName)

	return machineName, awsInstanceID, nil
}

func (o *controlPlane) patchMachineType(machine string, machineType string, reason string) error {
	printer.PrintlnGreen("Patching machine type of machine", machine, "to", machineType)
	err := bpelevate.RunElevate([]string{
		fmt.Sprintf("%s - Elevate required to patch machine type of machine %s to %s", reason, machine, machineType),
		`-n openshift-machine-api patch machine`, machine, `--patch "{\"spec\":{\"providerSpec\":{\"value\":{\"instanceType\":\"` + machineType + `\"}}}}" --type merge`,
	})
	if err != nil {
		return fmt.Errorf("could not patch machine type:\n%s", err)
	}
	return nil
}

type resizeControlPlaneNodeAWSClient interface {
	ec2.DescribeInstancesAPIClient
	StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	ModifyInstanceAttribute(ctx context.Context, params *ec2.ModifyInstanceAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error)
}

// run performs a control plane resize leveraging control plane machine sets
// https://docs.openshift.com/container-platform/latest/machine_management/control_plane_machine_management/cpmso-about.html
func (o *controlPlane) run(ctx context.Context) error {
	cpms := &machinev1.ControlPlaneMachineSet{}
	if err := o.client.Get(ctx, client.ObjectKey{Namespace: cpmsNamespace, Name: cpmsName}, cpms); err != nil {
		return fmt.Errorf("error retrieving control plane machine set: %v", err)
	}

	if cpms.Spec.State != machinev1.ControlPlaneMachineSetStateActive {
		return fmt.Errorf("control plane machine set is unexpectedly in %s state, must be %s - check for service logs, support exceptions, ask for a second opinion", cpms.Spec.State, machinev1.ControlPlaneMachineSetStateActive)
	}

	patch := client.MergeFrom(cpms.DeepCopy())

	var (
		rawBytes []byte
		err      error
	)
	switch o.cluster.CloudProvider().ID() {
	case "aws":
		awsSpec := &machinev1beta1.AWSMachineProviderConfig{}
		if err := json.Unmarshal(cpms.Spec.Template.OpenShiftMachineV1Beta1Machine.Spec.ProviderSpec.Value.Raw, &awsSpec); err != nil {
			return fmt.Errorf("error unmarshalling providerSpec: %v", err)
		}
		awsSpec.InstanceType = o.newMachineType

		rawBytes, err = json.Marshal(awsSpec)
		if err != nil {
			return fmt.Errorf("error marshalling awsSpec: %v", err)
		}
	case "gcp":
		gcpSpec := &machinev1beta1.GCPMachineProviderSpec{}
		if err := json.Unmarshal(cpms.Spec.Template.OpenShiftMachineV1Beta1Machine.Spec.ProviderSpec.Value.Raw, gcpSpec); err != nil {
			return fmt.Errorf("error unmarshalling providerSpec: %v", err)
		}

		gcpSpec.MachineType = o.newMachineType
		rawBytes, err = json.Marshal(gcpSpec)
		if err != nil {
			return fmt.Errorf("error marshalling gcpSpec: %v", err)
		}
	default:
		return fmt.Errorf("cloud provider not supported: %s, only AWS and GCP are supported", o.cluster.CloudProvider().ID())
	}

	log.Printf("Initiating control plane node resize for cluster %s/%s to %s using control plane machine sets. This process runs asynchronously.", o.cluster.Name(), o.cluster.ID(), o.newMachineType)
	if !utils.ConfirmPrompt() {
		return errors.New("aborting control plane resize")
	}

	// Patch the ControlPlaneMachineSet
	cpms.Spec.Template.OpenShiftMachineV1Beta1Machine.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: rawBytes}
	if err := o.clientAdmin.Patch(ctx, cpms, patch); err != nil {
		return fmt.Errorf("failed patching control plane machine set: %v", err)
	}

	log.Println("Control plane machine set patched successfully. The resize is now in progress and will complete asynchronously. This command will exit after sending a service log, and any issues will be reported via PagerDuty.")

	return promptGenerateResizeSL(o.clusterID, o.newMachineType)
}

func promptGenerateResizeSL(clusterID string, newMachineType string) error {
	fmt.Println("The resize operation is in progress and will complete asynchronously. A service log will now be sent to document this action. Any issues with the resize will be reported via PagerDuty.")
	fmt.Println("Would you like to proceed with sending the service log?")
	if !utils.ConfirmPrompt() {
		fmt.Println("Service log not sent. The resize is still in progress, and this command will now exit. Monitor PagerDuty for any issues.")
		return nil
	}

	var jiraID string
	fmt.Print("Please enter the JIRA ID that corresponds to this resize: ")
	_, err := fmt.Scanln(&jiraID)
	if err != nil {
		log.Printf("Error reading JIRA ID: %v, proceeding with empty value", err)
	}

	var justification string
	fmt.Print("Please enter a justification for the resize: ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		justification = scanner.Text()
	} else if err := scanner.Err(); err != nil {
		errText := "failed to read justification text, send service log manually"
		_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n", errText, err)
		return errors.New(errText)
	}

	postCmd := servicelog.PostCmdOptions{
		Template: resizeControlPlaneServiceLogTemplate,
		TemplateParams: []string{
			fmt.Sprintf("INSTANCE_TYPE=%s", newMachineType),
			fmt.Sprintf("JIRA_ID=%s", jiraID),
			fmt.Sprintf("JUSTIFICATION=%s", justification),
		},
		ClusterId: clusterID,
	}

	if err := postCmd.Run(); err != nil {
		return fmt.Errorf("failed to send service log: %v", err)
	}

	fmt.Println("Service log sent successfully. Use the following command to track progress of the resize:")
	fmt.Println()
	fmt.Println(`watch -d 'oc get machines -n openshift-machine-api -l machine.openshift.io/cluster-api-machine-role=master && oc get nodes -l node-role.kubernetes.io/control-plane'`)

	return nil
}
