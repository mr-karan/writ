package provider

import (
	"strings"
	"testing"
)

func TestAWSReferenceTypes(t *testing.T) {
	p, _, err := buildSchema(awsSrc, "6.70.0", awsFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	vpc := "Id<aws.DefaultVpc | aws.Vpc>"
	subnet := "Id<aws.DefaultSubnet | aws.Subnet>"
	sg := "Id<aws.DefaultSecurityGroup | aws.SecurityGroup>"
	role := "Arn<aws.IamRole | aws.IamServiceLinkedRole>"
	kms := "Arn<aws.KmsExternalKey | aws.KmsKey | aws.KmsReplicaExternalKey | aws.KmsReplicaKey>"
	tests := []struct {
		wire, path, want string
	}{
		{"aws_subnet", "id", "Id<aws.Subnet>"},
		{"aws_default_subnet", "id", "Id<aws.DefaultSubnet>"},
		{"aws_subnet", "vpc_id", vpc},
		{"aws_security_group", "vpc_id", vpc},
		{"aws_lb_target_group", "vpc_id", vpc},
		{"aws_vpc", "default_security_group_id", sg},
		{"aws_lb", "subnets", "[" + subnet + "]"},
		{"aws_lb", "subnet_mapping.subnet_id", subnet},
		{"aws_lb", "security_groups", "[" + sg + "]"},
		{"aws_autoscaling_group", "vpc_zone_identifier", "[" + subnet + "]"},
		{"aws_autoscaling_group", "target_group_arns", "[Arn<aws.AlbTargetGroup | aws.LbTargetGroup>]"},
		{"aws_autoscaling_group", "launch_template.id", "Id<aws.LaunchTemplate>"},
		{"aws_autoscaling_group", "mixed_instances_policy.launch_template.launch_template_specification.launch_template_id", "Id<aws.LaunchTemplate>"},
		{"aws_vpc_endpoint_route_table_association", "route_table_id", "Id<aws.DefaultRouteTable | aws.RouteTable>"},
		{"aws_ebs_snapshot", "volume_id", "Id<aws.EbsVolume>"},
		{"aws_volume_attachment", "volume_id", "Id<aws.EbsVolume>"},
		{"aws_volume_attachment", "instance_id", "Id<aws.Instance>"},
		{"aws_iam_instance_profile", "role", "Name<aws.IamRole>"},
		{"aws_iam_role", "id", "Name<aws.IamRole>"},
		{"aws_iam_role", "name", "String"},
		{"aws_iam_role", "arn", "Arn<aws.IamRole>"},
		// A service-linked role's id is its ARN.
		{"aws_iam_service_linked_role", "id", "Arn<aws.IamServiceLinkedRole>"},
		{"aws_iam_service_linked_role", "arn", "Arn<aws.IamServiceLinkedRole>"},
		// Slots named for a role take any role ARN.
		{"aws_ecs_task_definition", "execution_role_arn", role},
		{"aws_ecs_task_definition", "task_role_arn", role},
		{"aws_db_instance", "monitoring_role_arn", role},
		{"aws_eks_node_group", "node_role_arn", role},
		{"aws_eks_node_group", "remote_access.source_security_group_ids", "[" + sg + "]"},
		{"aws_security_group_rule", "source_security_group_id", sg},
		{"aws_security_group_rule", "security_group_id", sg},
		{"aws_kms_key", "id", "Id<aws.KmsKey>"},
		{"aws_kms_key", "arn", "Arn<aws.KmsKey>"},
		{"aws_dms_endpoint", "kms_key_arn", kms},
		// A KMS key slot named <x>_id takes the key's id, its ARN, or an
		// alias, and a typed reference has one kind.
		{"aws_kms_alias", "target_key_id", "String"},
		{"aws_db_instance", "kms_key_id", "String"},
		// The service's own type of the slot's name wins: this is a DMS
		// certificate, not an ACM one.
		{"aws_dms_endpoint", "certificate_arn", "String"},
		// A VPC Lattice target group, kept plain by the annotation table.
		{"aws_ecs_service", "vpc_lattice_configurations.target_group_arn", "String"},
		{"aws_ecs_service", "load_balancer.target_group_arn", "Arn<aws.AlbTargetGroup | aws.LbTargetGroup>"},
		{"aws_s3_bucket", "id", "Name<aws.S3Bucket>"},
		{"aws_s3_bucket", "arn", "Arn<aws.S3Bucket>"},
		{"aws_lb", "id", "Arn<aws.Lb>"},
		{"aws_lb", "arn", "Arn<aws.Lb>"},
		{"aws_lb_target_group", "arn", "Arn<aws.LbTargetGroup>"},
		{"aws_lb", "zone_id", "String"},
		{"aws_route53_record", "zone_id", "Id<aws.Route53Zone>"},
		// An alias names its target's hosted zone, such as a load balancer's.
		{"aws_route53_record", "alias.zone_id", "String"},
		// A type's own <x>_id is its identifier, not a reference.
		{"aws_route53_zone", "zone_id", "String"},
		// The most specific candidate is aws_connect_instance, which is in no
		// family, so this is not taken for an EC2 instance.
		{"aws_connect_queue", "instance_id", "String"},
		// aws_appsync_api exists but this slot takes a GraphQL API id.
		{"aws_appsync_datasource", "api_id", "String"},
		// A KMS key slot accepts key ids, ARNs, and aliases.
		{"aws_ebs_volume", "kms_key_id", "String"},
	}
	for _, tt := range tests {
		t.Run(tt.wire+"."+tt.path, func(t *testing.T) {
			r, err := p.ResourceByWire(tt.wire)
			if err != nil {
				t.Fatal(err)
			}
			a, ok := r.Block.Lookup(strings.Split(tt.path, "."))
			if !ok {
				t.Fatalf("%s has no attribute %s", tt.wire, tt.path)
			}
			if got := a.Type.String(); got != tt.want {
				t.Errorf("type = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestAWSIdIsReadOnly(t *testing.T) {
	p, _, err := buildSchema(awsSrc, "6.70.0", awsFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire("aws_vpc")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := r.Block.Lookup([]string{"id"})
	if !ok || !readOnly(a) {
		t.Errorf("aws.Vpc id = %+v, %v; SDKv2 marks it optional, Writ treats it as read-only, so a resource body cannot set it", a, ok)
	}
}

// Data sources are typed like resources, so a looked-up VPC's id flows into
// a subnet's vpc_id and a list of subnet ids into a load balancer.
func TestAWSDataSourceReferenceTypes(t *testing.T) {
	p, _, err := buildSchema(awsSrc, "6.70.0", awsFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		wire, path, want string
	}{
		{"aws_vpc", "id", "Id<aws.Vpc>"},
		{"aws_vpc", "dhcp_options_id", "Id<aws.DefaultVpcDhcpOptions | aws.VpcDhcpOptions>"},
		{"aws_subnet", "id", "Id<aws.Subnet>"},
		{"aws_subnet", "vpc_id", "Id<aws.DefaultVpc | aws.Vpc>"},
		{"aws_subnets", "ids", "[Id<aws.DefaultSubnet | aws.Subnet>]"},
		{"aws_ami", "id", "Id<aws.Ami>"},
		// No resource type shares these names, so their ids are plain.
		{"aws_region", "id", "String"},
		{"aws_partition", "id", "String"},
	}
	for _, tt := range tests {
		t.Run(tt.wire+"."+tt.path, func(t *testing.T) {
			d, err := p.DataSourceByWire(tt.wire)
			if err != nil {
				t.Fatal(err)
			}
			a, ok := d.Block.Lookup(strings.Split(tt.path, "."))
			if !ok {
				t.Fatalf("%s has no attribute %s", tt.wire, tt.path)
			}
			if got := a.Type.String(); got != tt.want {
				t.Errorf("type = %s, want %s", got, tt.want)
			}
		})
	}
}

// The Scaled entries name settable attributes of the real 6.70.0 schema, so
// none of them is skipped as missing.
func TestAWSScaledFields(t *testing.T) {
	p, _, err := buildSchema(awsSrc, "6.70.0", awsFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for wire, want := range map[string]string{
		"aws_autoscaling_group": "desired_capacity",
		"aws_ecs_service":       "desired_count",
	} {
		r, err := p.ResourceByWire(wire)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Scaled) != 1 || r.Scaled[0] != want {
			t.Errorf("%s Scaled = %v, want [%s]", wire, r.Scaled, want)
		}
	}
}
