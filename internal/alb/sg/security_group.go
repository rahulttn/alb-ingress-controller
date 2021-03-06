package sg

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albec2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/log"
)

// SecurityGroup represents an SecurityGroup resource in AWS
type SecurityGroup struct {
	// We identify SecurityGroup either by GroupID or GroupName
	GroupID   *string
	GroupName *string

	InboundPermissions []*ec2.IpPermission
}

// SecurityGroupController manages SecurityGroups
type SecurityGroupController interface {
	// Reconcile ensures the securityGroup exists and match the specification.
	// Field GroupID or GroupName will be populated if unspecified.
	Reconcile(*SecurityGroup) error

	// Delete ensures the securityGroup does not exist.
	Delete(*SecurityGroup) error
}

type securityGroupController struct {
	ec2    albec2.EC2API
	logger *log.Logger
}

func (controller *securityGroupController) Reconcile(group *SecurityGroup) error {
	instance, err := controller.findExistingSGInstance(group)
	if err != nil {
		return err
	}
	if instance != nil {
		return controller.reconcileByModifySGInstance(group, instance)
	}
	return controller.reconcileByNewSGInstance(group)
}

func (controller *securityGroupController) Delete(group *SecurityGroup) error {
	if group.GroupID != nil {
		controller.logger.Infof("deleting securityGroup %s", aws.StringValue(group.GroupID))
		return controller.ec2.DeleteSecurityGroupByID(*group.GroupID)
	}
	instance, err := controller.findExistingSGInstance(group)
	if err != nil {
		return err
	}
	if instance != nil {
		controller.logger.Infof("deleting securityGroup %s", aws.StringValue(instance.GroupId))
		return controller.ec2.DeleteSecurityGroupByID(*instance.GroupId)
	}
	return nil
}

func (controller *securityGroupController) reconcileByNewSGInstance(group *SecurityGroup) error {
	// TODO: move these VPC calls into controller startup, and part of controller configuration
	vpcID, err := controller.ec2.GetVPCID()
	if err != nil {
		return err
	}
	createSGOutput, err := controller.ec2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       vpcID,
		GroupName:   group.GroupName,
		Description: aws.String("Instance SecurityGroup created by alb-ingress-controller"),
	})
	if err != nil {
		return err
	}
	group.GroupID = createSGOutput.GroupId

	_, err = controller.ec2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       group.GroupID,
		IpPermissions: group.InboundPermissions,
	})
	if err != nil {
		return err
	}

	_, err = controller.ec2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{group.GroupID},
		Tags: []*ec2.Tag{
			{
				Key:   aws.String("Name"),
				Value: group.GroupName,
			},
			{
				Key:   aws.String(albec2.ManagedByKey),
				Value: aws.String(albec2.ManagedByValue),
			},
		},
	})
	if err != nil {
		return err
	}
	controller.logger.Infof("created new securityGroup %s", aws.StringValue(group.GroupID))

	return nil
}

// reconcileByModifySGInstance modified the sg intance in AWS to match the specification specified in group
func (controller *securityGroupController) reconcileByModifySGInstance(group *SecurityGroup, instance *ec2.SecurityGroup) error {
	if group.GroupID == nil {
		group.GroupID = instance.GroupId
	}
	if group.GroupName == nil {
		group.GroupName = instance.GroupName
	}

	permissionsToRevoke := diffIPPermissions(instance.IpPermissions, group.InboundPermissions)
	if len(permissionsToRevoke) != 0 {
		controller.logger.Infof("revoking inbound permissions from securityGroup %s", aws.StringValue(group.GroupID))
		_, err := controller.ec2.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
			GroupId:       group.GroupID,
			IpPermissions: permissionsToRevoke,
		})
		if err != nil {
			return fmt.Errorf("failed to revoke inbound permissions due to %v", err)
		}
	}

	permissionsToGrant := diffIPPermissions(group.InboundPermissions, instance.IpPermissions)
	if len(permissionsToGrant) != 0 {
		controller.logger.Infof("granting inbound permissions to securityGroup %s", aws.StringValue(group.GroupID))
		_, err := controller.ec2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       group.GroupID,
			IpPermissions: permissionsToGrant,
		})
		if err != nil {
			return fmt.Errorf("failed to grant inbound permissions due to %v", err)
		}
	}
	return nil
}

// findExistingSGInstance tring to find the existing SG matches the specification
func (controller *securityGroupController) findExistingSGInstance(group *SecurityGroup) (*ec2.SecurityGroup, error) {
	switch {
	case group.GroupID != nil:
		{
			instance, err := controller.ec2.GetSecurityGroupByID(aws.StringValue(group.GroupID))
			if err != nil {
				return nil, err
			}
			if instance == nil {
				return nil, fmt.Errorf("securityGroup %s doesn't exist", aws.StringValue(group.GroupID))
			}
			return instance, nil
		}
	case group.GroupName != nil:
		{
			vpcID, err := controller.ec2.GetVPCID()
			if err != nil {
				return nil, err
			}
			instance, err := controller.ec2.GetSecurityGroupByName(aws.StringValue(vpcID), aws.StringValue(group.GroupName))
			if err != nil {
				return nil, err
			}
			return instance, nil
		}
	}
	return nil, fmt.Errorf("Either GroupID or GroupName must be specified")
}

// diffIPPermissions calcutes set_difference as source - target
func diffIPPermissions(source []*ec2.IpPermission, target []*ec2.IpPermission) (diffs []*ec2.IpPermission) {
	for _, sPermission := range source {
		containsInTarget := false
		for _, tPermission := range target {
			if ipPermissionEquals(sPermission, tPermission) {
				containsInTarget = true
				break
			}
		}
		if containsInTarget == false {
			diffs = append(diffs, sPermission)
		}
	}
	return diffs
}

// ipPermissionEquals test whether two IPPermission instance are equals
func ipPermissionEquals(source *ec2.IpPermission, target *ec2.IpPermission) bool {
	if aws.StringValue(source.IpProtocol) != aws.StringValue(target.IpProtocol) {
		return false
	}
	if aws.Int64Value(source.FromPort) != aws.Int64Value(target.FromPort) {
		return false
	}
	if aws.Int64Value(source.ToPort) != aws.Int64Value(target.ToPort) {
		return false
	}
	if len(diffIPRanges(source.IpRanges, target.IpRanges)) != 0 {
		return false
	}
	if len(diffIPRanges(target.IpRanges, source.IpRanges)) != 0 {
		return false
	}
	if len(diffUserIDGroupPairs(source.UserIdGroupPairs, target.UserIdGroupPairs)) != 0 {
		return false
	}
	if len(diffUserIDGroupPairs(target.UserIdGroupPairs, source.UserIdGroupPairs)) != 0 {
		return false
	}

	return true
}

// diffIPRanges calcutes set_difference as source - target
func diffIPRanges(source []*ec2.IpRange, target []*ec2.IpRange) (diffs []*ec2.IpRange) {
	for _, sRange := range source {
		containsInTarget := false
		for _, tRange := range target {
			if ipRangeEquals(sRange, tRange) {
				containsInTarget = true
				break
			}
		}
		if containsInTarget == false {
			diffs = append(diffs, sRange)
		}
	}
	return diffs
}

// ipRangeEquals test whether two IPRange instance are equals
func ipRangeEquals(source *ec2.IpRange, target *ec2.IpRange) bool {
	return aws.StringValue(source.CidrIp) == aws.StringValue(target.CidrIp)
}

// diffUserIDGroupPairs calcutes set_difference as source - target
func diffUserIDGroupPairs(source []*ec2.UserIdGroupPair, target []*ec2.UserIdGroupPair) (diffs []*ec2.UserIdGroupPair) {
	for _, sPair := range source {
		containsInTarget := false
		for _, tPair := range target {
			if userIDGroupPairEquals(sPair, tPair) {
				containsInTarget = true
				break
			}
		}
		if containsInTarget == false {
			diffs = append(diffs, sPair)
		}
	}
	return diffs
}

// userIDGroupPairEquals test whether two UserIdGroupPair equals
// currently we only check for groupId
func userIDGroupPairEquals(source *ec2.UserIdGroupPair, target *ec2.UserIdGroupPair) bool {
	if aws.StringValue(source.GroupId) != aws.StringValue(target.GroupId) {
		return false
	}
	return true
}
