package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/cloudprovider"
	awscp "github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/cloudprovider/aws"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/resource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	// Get kubeconfig, AWS credentials, and artifact dir from environment variable set by the OpenShift CI operator.
	kubeconfig     = os.Getenv("KUBECONFIG")
	awscredentials = os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	artifactDir    = os.Getenv("ARTIFACT_DIR")
	privateKeyPath = os.Getenv("KUBE_SSH_KEY_PATH")

	// imageID is the image that will be fed to the WNI for the tests. This is being set to empty, as we wish for it
	// to use the latest Windows image
	imageID      = ""
	instanceType = "m4.large"
	sshKey       = "libra"

	// awsProvider is setup as a variable for both creating, destroying,
	// and tear down Windows instance in case test fails in the middle.
	awsProvider = &awscp.AwsProvider{}

	// Set global variables for instance object, instance, security group,
	// and infrastructure IDs so that once they are created,
	// they will be used by all subsequent testing functions.
	createdInstance   = &ec2.Instance{}
	createdInstanceID = ""
	createdSgID       = ""
	infraID           = ""
)

// TestAwsE2eSerial runs all e2e tests for the AWS implementation serially. It creates the Windows instance,
// checks all properties of the instance and destroy the instance and check that resource are deleted.
func TestAwsE2eSerial(t *testing.T) {
	err := awsSetup()
	if err != nil {
		tdErr := tearDownInstance()
		if tdErr != nil {
			t.Logf("error with test teardown: %s", tdErr)
		}
		t.Fatal(err)
	}

	t.Run("test create Windows Instance", testCreateWindowsInstance)

	t.Run("test destroy Windows instance", testDestroyWindowsInstance)

	// Make sure the instance is torn down in case the destroy fails
	err = tearDownInstance()
	if err != nil {
		t.Logf("error with test teardown: %s", err)
	}
}

// testCreateWindowsInstance tests the creation of a Windows instance and checks its properties and attached items.
func testCreateWindowsInstance(t *testing.T) {
	t.Run("test proper AMI was used", testImageUsed)
	t.Run("test if instance status is ok", testInstanceStatusOk)
	t.Run("test created instance properties", testInstanceProperties)
	t.Run("test instance is attached a public subnet", testInstanceHasPublicSubnetAndIp)
	t.Run("test instance has name tag", testInstanceIsAttachedWithName)
	t.Run("test instance has infrastructure tag", testInstanceInfraTagExists)
	t.Run("test instance is attached the cluster worker's security group", testInstanceHasClusterWorkerSg)
	t.Run("test instance is attache a Windows security group", testInstanceIsAssociatedWithWindowsWorkerSg)
	t.Run("test instance is associated with cluster worker's IAM", testInstanceIsAssociatedWithClusterWorkerIAM)
	t.Run("test container logs port is open in Windows firewall", testInstanceFirewallRule)
}

// testDestroyWindowsInstance tests the deletion of a Windows instance and checks if the created instance and Windows
// security group are deleted.
func testDestroyWindowsInstance(t *testing.T) {
	t.Run("test instance is terminated", destroyingWindowsInstance)
	t.Run("test Windows security group is deleted", testSgIsDeleted)
	t.Run("test installer json file is deleted", testInstallerJsonFileIsDeleted)

}

// awsSetup does the setup steps such as:
// 1. Obtain the awsProvider object from the cloud factory implementation.
// 2. Spin up the windows instance, to test properties on it.
func awsSetup() error {
	err := setupAWSCloudProvider()
	if err != nil {
		return err
	}
	err = setupWindowsInstanceWithResources()
	if err != nil {
		return err
	}
	return nil
}

// setupAWSCloudProvider creates provider ofr Cloud interface and asserts type into AWS provider.
// This is the first step of the e2e test and fails the test upon error.
func setupAWSCloudProvider() error {
	// The e2e test uses Microsoft Windows Server 2019 Base with Containers image, m4.large type, and libra ssh key.
	cloud, err := cloudprovider.CloudProviderFactory(kubeconfig, awscredentials, "default", artifactDir,
		imageID, instanceType, sshKey, privateKeyPath)
	if err != nil {
		return fmt.Errorf("error obtaining aws interface object: %s", err)
	}

	// Type assert to AWS so that we can test other functionality
	provider, ok := cloud.(*awscp.AwsProvider)
	if !ok {
		return fmt.Errorf("error asserting cloudprovider to awsProvider")
	}
	awsProvider = provider
	return nil
}

// setupWindowsInstanceWithResources creates a Windows instance and updates global information for infraID,
// createdInstanceID, and createdSgID. All information updates are required to be successful or instance will be
// teared down.
func setupWindowsInstanceWithResources() error {
	var err error
	// Create the instance
	credentials, err = awsProvider.CreateWindowsVM()
	if err != nil {
		return fmt.Errorf("error creating Windows instance: %s", err)
	}

	// Ensure we have the login info for the instance
	if credentials == nil {
		return fmt.Errorf("returned credentials empty")
	}
	if credentials.GetPassword() == "" {
		return fmt.Errorf("returned password empty")
	}
	if credentials.GetInstanceId() == "" {
		return fmt.Errorf("returned instance id empty")
	}

	infraID, err = awsProvider.GetInfraID()
	if err != nil {
		return fmt.Errorf("error while getting infrastructure ID for the OpenShift cluster: %s", err)
	}

	// Check instance and security group information in windows-node-installer.json.
	info, err := resource.ReadInstallerInfo(artifactDir + "/" + "windows-node-installer.json")
	if err != nil {
		return fmt.Errorf("error reading from windows-node-installer.json file: %s", err)
	}

	// Set security group value
	if len(info.SecurityGroupIDs) != 1 {
		return fmt.Errorf("expected one security group but found %d", len(info.SecurityGroupIDs))
	}
	if info.SecurityGroupIDs[0] == "" {
		return fmt.Errorf("found empty security group")
	}
	createdSgID = info.SecurityGroupIDs[0]

	// Set instanceID value
	if len(info.InstanceIDs) != 1 {
		return fmt.Errorf("expected one instance but found %d", len(info.InstanceIDs))
	}
	if info.InstanceIDs[0] == "" {
		return fmt.Errorf("found empty instance id value")
	}
	createdInstanceID = info.InstanceIDs[0]

	instance, err := getInstance(createdInstanceID)
	if err != nil {
		return fmt.Errorf("could not resolve instance id %s to instance: %s", createdInstanceID, err)
	}
	createdInstance = instance
	return nil
}

// waitForStatusok waits for the instance to be okay.
func waitForStatusOk(instanceId string) error {
	for i := 0; i < retryCount; i++ {
		instanceStatus, err := getInstanceStatus(instanceId)
		if err != nil {
			fmt.Errorf("failed to get the status of the instance: %v", err)
		}
		if instanceStatus == "ok" {
			return nil
		}
		time.Sleep(retryInterval)
	}
	return fmt.Errorf("failed to obtain the ok status")
}

// getInstanceStatus returns the status of the instance.
func getInstanceStatus(instanceId string) (string, error) {
	ec2Svc := awsProvider.EC2
	input := &ec2.DescribeInstanceStatusInput{
		DryRun:              nil,
		Filters:             nil,
		IncludeAllInstances: nil,
		InstanceIds: []*string{
			&instanceId,
		},
		MaxResults: nil,
		NextToken:  nil,
	}
	result, err := ec2Svc.DescribeInstanceStatus(input)
	if err != nil {
		return "", fmt.Errorf("failed to DescribeInstanceStatus with error: %v", err)
	}
	if result.InstanceStatuses == nil {
		return "", fmt.Errorf("InstanceStatuses is nil")
	}

	// currently we are creating only a single instance which is going to be the
	// zeroth entry in it.
	if result.InstanceStatuses[0] == nil {
		return "", fmt.Errorf("InstanceStatuses[0] is nil")
	}
	if result.InstanceStatuses[0].InstanceStatus == nil {
		return "", fmt.Errorf("windows InstanceStatus is nil")
	}
	if result.InstanceStatuses[0].InstanceStatus.Status == nil {
		return "", fmt.Errorf("windows InstanceStatus.Status field is nil")
	}
	instanceStatus := *result.InstanceStatuses[0].InstanceStatus.Status
	return instanceStatus, nil
}

// testInstanceStatusOk tests if instance status is ok or not.
func testInstanceStatusOk(t *testing.T) {
	err := waitForStatusOk(credentials.GetInstanceId())
	require.NoErrorf(t, err, "failed to get the instance status as ok")
}

// getInstance gets the instance information from AWS based on instance ID and returns errors if fails.
func getInstance(instanceID string) (*ec2.Instance, error) {
	instances, err := awsProvider.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	})

	if err != nil {
		return nil, err
	}
	if len(instances.Reservations) < 1 || len(instances.Reservations[0].Instances) < 1 {
		return nil, fmt.Errorf("instance does not exist")
	}
	return instances.Reservations[0].Instances[0], err
}

// tearDownInstance removes the lingering resources including instance and Windows security group when required steps of
// the test fail.
func tearDownInstance() error {
	if createdInstanceID != "" {
		err := awsProvider.TerminateInstance(createdInstanceID)
		if err != nil {
			return fmt.Errorf("error terminating instance during teardown, %v", err)
		}
	}
	// Return the global variables to their original state to no longer reference the torn down instance
	createdInstanceID = ""
	createdInstance = &ec2.Instance{}

	if createdSgID != "" {
		err := awsProvider.DeleteSG(createdSgID)
		if err != nil {
			return fmt.Errorf("error deleting security group during teardown, %v", err)
		}
	}
	return nil
}

// testImageUsed tests that the proper Windows AMI was used
func testImageUsed(t *testing.T) {
	describedImages, err := awsProvider.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{createdInstance.ImageId},
	})
	require.NoErrorf(t, err, "Could not describe images with imageID: %s", createdInstance.ImageId)
	require.Lenf(t, describedImages.Images, 1, "Found unexpected amount of AMIs with imageID %s",
		createdInstance.ImageId)

	foundImage := describedImages.Images[0]
	require.Contains(t, *foundImage.Name, "Windows_Server-2019-English-Full-ContainersLatest")
}

// testInstanceProperties updates the createdInstance global object and asserts if an instance is in the running
// state, has the right image id, instance type, and ssh key associated.
func testInstanceProperties(t *testing.T) {
	assert.Equal(t, ec2.InstanceStateNameRunning, *createdInstance.State.Name,
		"created instance is not in running state")

	assert.Equalf(t, instanceType, *createdInstance.InstanceType, "created instance type mismatch")

	assert.Equalf(t, sshKey, *createdInstance.KeyName, "created instance ssh key mismatch")
}

// testInstanceHasPublicSubnetAndIp asserts if the instance is associated with a public IP address and subnet by
// checking if the subnet routing table has internet gateway attached.
func testInstanceHasPublicSubnetAndIp(t *testing.T) {
	assert.NotEmpty(t, createdInstance.PublicIpAddress, "instance does not have a public IP address")

	routeTables, err := awsProvider.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("association.subnet-id"),
				Values: []*string{createdInstance.SubnetId},
			},
		},
	})
	if err != nil || len(routeTables.RouteTables) < 1 {
		assert.Fail(t, fmt.Sprintf("error finding route table for subnet %s, %v", *createdInstance.SubnetId, err))
		return
	}

	for _, route := range routeTables.RouteTables[0].Routes {
		igws, err := awsProvider.EC2.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
			InternetGatewayIds: []*string{route.GatewayId},
		})
		if err == nil && len(igws.InternetGateways) > 0 {
			return
		}
	}
	assert.Fail(t, "subnet associated is not a public subnet")
}

// testInstanceIsAttachedWithName asserts if an instance has a Name tag value.
// Instance input needs to be updated before use.
func testInstanceIsAttachedWithName(t *testing.T) {
	for _, tag := range createdInstance.Tags {
		if *tag.Key == "Name" && tag.Value != nil {
			return
		}
	}
	assert.Fail(t, "instance is not assigned a name")
}

// testInstanceInfraTagExists asserts if the infrastructure tag exists on the created instance.
// Instance input needs to be updated before use.
func testInstanceInfraTagExists(t *testing.T) {
	key := "kubernetes.io/cluster/" + infraID
	value := "owned"
	for _, tag := range createdInstance.Tags {
		if *tag.Key == key && *tag.Value == value {
			return
		}
	}
	assert.Fail(t, "infrastructure tag not found")
}

// testInstanceHasClusterWorkerSg asserts if the created instance has OpenShift cluster worker security group attached.
func testInstanceHasClusterWorkerSg(t *testing.T) {
	workerSg, err := awsProvider.GetClusterWorkerSGID(infraID)
	assert.NoError(t, err, "failed to get OpenShift cluster worker security group")

	for _, sg := range createdInstance.SecurityGroups {
		if *sg.GroupId == workerSg {
			return
		}
	}
	assert.Fail(t, "instance is not associated with OpenShift cluster worker security group")
}

// testInstanceIsAssociatedWithWindowsWorkerSg asserts if the created instance has a security group made for the
// Windows instance attached by checking the group name, recorded id, necessary ports, and ip-permission.cidr.
func testInstanceIsAssociatedWithWindowsWorkerSg(t *testing.T) {
	myIp, err := awscp.GetMyIp()
	assert.NoError(t, err, "error getting user's public IP")

	vpc, err := awsProvider.GetVPCByInfrastructure(infraID)
	if err != nil {
		assert.Fail(t, "error getting OpenShift cluster VPC, %v", err)
		return
	}

	sgs, err := awsProvider.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("ip-permission.from-port"),
				Values: aws.StringSlice([]string{"-1", "3389"}),
			},
			{
				Name:   aws.String("ip-permission.to-port"),
				Values: aws.StringSlice([]string{"-1", "3389"}),
			},
			{
				Name:   aws.String("ip-permission.from-port"),
				Values: aws.StringSlice([]string{"-1", "22"}),
			},
			{
				Name:   aws.String("ip-permission.to-port"),
				Values: aws.StringSlice([]string{"-1", "22"}),
			},
			{
				Name:   aws.String("ip-permission.protocol"),
				Values: aws.StringSlice([]string{"tcp"}),
			},
			{
				Name:   aws.String("group-id"),
				Values: aws.StringSlice([]string{createdSgID}),
			},
			{
				Name:   aws.String("ip-permission.cidr"),
				Values: aws.StringSlice([]string{myIp + "/32", *vpc.CidrBlock}),
			},
		},
	})
	if err != nil || len(sgs.SecurityGroups) < 1 {
		assert.Fail(t, "instance is not associated with a Windows security group, %v", err)
	}
}

// testInstanceIsAssociatedWithClusterWorkerIAM asserts if the created instance has the OpenShift cluster worker's IAM
// attached.
func testInstanceIsAssociatedWithClusterWorkerIAM(t *testing.T) {
	iamProfile, err := awsProvider.GetIAMWorkerRole(infraID)
	assert.NoError(t, err, "error getting OpenShift Cluster Worker IAM")

	assert.Equal(t, *iamProfile.Arn, *createdInstance.IamInstanceProfile.Arn, "instance is not associated with worker IAM profile")
}

// destroyingWindowsInstance destroys Windows instance and updates the createdInstance global object.
func destroyingWindowsInstance(t *testing.T) {
	err := awsProvider.DestroyWindowsVMs()
	require.NoError(t, err, "Error destroying Windows VMs")

	createdInstance, err = getInstance(createdInstanceID)
	require.NoError(t, err, "Error retrieving Windows VM")

	assert.Equal(t, ec2.InstanceStateNameTerminated, *createdInstance.State.Name,
		"instance is not in the terminated state")
}

// testSgIsDeleted asserts if a security group is deleted by checking whether the security group exist on AWS.
// If delete is successful, the id in createdSgID is erased.
func testSgIsDeleted(t *testing.T) {
	sgs, err := awsProvider.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: aws.StringSlice([]string{createdSgID}),
	})

	if err == nil && len(sgs.SecurityGroups) > 0 {
		assert.Fail(t, "security group is not deleted")
	} else {
		createdSgID = ""
	}
}

// testInstallerJsonFileIsDeleted asserts that the windows-node-installer.json is deleted.
func testInstallerJsonFileIsDeleted(t *testing.T) {
	// the windows-node-installer.json should be removed after resource is deleted.
	_, err := resource.ReadInstallerInfo(artifactDir)
	assert.Error(t, err, "error deleting windows-node-installer.json file")
}
