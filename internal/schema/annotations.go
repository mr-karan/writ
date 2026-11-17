package schema

import "github.com/mr-karan/writ/internal/types"

// providerAnnotations refines one provider's resource types.
type providerAnnotations struct {
	Types map[string]annotation // by wire type name
	Data  map[string]annotation // by wire data source name
	// Families group resource types whose computed ids share one namespace,
	// such as aws_vpc and aws_default_vpc. A reference to one member accepts
	// the id of any member. A family's Kind is the kind of reference its
	// members' ids are, and of their arn attribute when Kind is ArnRef.
	//
	// Name inference (see refIndex.infer) types only references to a
	// family, because a family is a promise that every value the slot
	// accepts comes from one of its members. A resource type that is in no
	// family is never a target, so its <x>_id and <x>_arn slots stay plain
	// strings.
	Families []family
	// Names maps an attribute name that inference cannot read, in any
	// resource type, to the family member it refers to.
	Names map[string]string
	// Suffixes maps the end of a slot name, in its singular form, to the
	// family members a slot whose name ends with it refers to: a slot named
	// execution_role_arn or role_arns ends with role_arn. The longest entry
	// wins. An entry with no members keeps a slot plain where a shorter
	// entry would type it.
	Suffixes map[string][]string
}

type family struct {
	Kind  types.RefKind
	Wires []string
	// Arn types each member's computed arn as an Arn reference to it, for a
	// family whose ids are not ARNs, such as IAM roles, whose ids are names.
	// Inference then types <x>_arn slots for the family.
	Arn bool
	// IdTakesArn keeps <x>_id slots for the family plain, because its API
	// takes the ARN wherever it takes the id, as KMS does. A typed
	// reference has one kind, so an Id slot would refuse the ARN.
	IdTakesArn bool
}

// annotation refines one resource type. Attrs maps an attribute path
// (subject.common_name) to the Writ type that replaces the schema's String.
type annotation struct {
	// SDKv2 omits disabled conditions and computed block flags from the protocol.
	ImportDisabled map[string]DisabledWhen
	ImportOwners   map[string]InlineOwner
	Attrs          map[string]types.Type
	Secret         []string
	// Single lists list blocks the provider limits to one item in a validator
	// instead of in the schema, so Writ shows them as records.
	Single []string
	// Refs maps an attribute path to the family member it refers to, for
	// names inference cannot read. The reference kind is the family's.
	Refs map[string]string
	// Plain lists attribute paths that inference would type but that accept
	// values from outside the inferred family.
	Plain []string
	// Filled lists top-level computed attributes that the provider sets to
	// a non-null value when config leaves them null. Writ reads an unset
	// one as non-null. Any other unset computed attribute reads as nullable,
	// because a provider may leave it null: tls 4.4.2 leaves max_path_length
	// null. Add an attribute only with a provider source line or a real run
	// that shows the value; TestFilledAttributesExist checks each name.
	Filled []string
	// ReadDefaults holds JSON defaults for provider-only settings that Read
	// never fills. Evidence must cover both the default and the read behavior.
	ReadDefaults        map[string]string
	ReadDefaultsVersion string
	SettlingReadVersion string
	SettingsVersion     string
	Settings            *SettingsRule
	Label               *Label
}

// annotations returns the whole annotation layer: one table, keyed by
// provider source and wire type name. To type a new attribute, add one line
// here and run `go test ./internal/schema`, which checks every entry against
// the attribute's base type. It is a function, not a package variable, so a
// bad entry fails a test instead of panicking at program start.
func annotations() map[Source]providerAnnotations {
	randomString := []string{
		"id", "result", "special", "upper", "lower", "number", "numeric",
		"min_lower", "min_upper", "min_numeric", "min_special",
	}
	localFile := []string{
		"id", "file_permission", "directory_permission",
		"content_md5", "content_sha1", "content_sha256", "content_sha512",
		"content_base64sha256", "content_base64sha512",
	}
	// The read of local v2.9.1 sets every computed attribute with
	// types.StringValue (data_source_local_file.go:106-118), and the
	// sensitive file calls the same read (data_source_local_sensitive_file.go:87).
	localFileData := annotation{Filled: []string{
		"id", "content", "content_base64", "content_md5", "content_sha1", "content_sha256",
		"content_sha512", "content_base64sha256", "content_base64sha512",
	}}
	return map[Source]providerAnnotations{
		{Namespace: "hashicorp", Type: "aws"}: aws(),
		// Filled: each Create in tls v4.4.2 sets these, or the schema gives
		// a Default. An apply on 2026-11-11 that left every optional
		// attribute unset gave a null max_path_length and no other null
		// computed attribute. A P224 key gets empty OpenSSH strings.
		{Namespace: "hashicorp", Type: "tls"}: {Data: map[string]annotation{
			// data_source_certificate.go:205-216 sets a list built from a
			// non-nil slice and the id.
			"tls_certificate": {Filled: []string{"id", "certificates"}},
			// data_source_public_key.go:140 and common_key.go:186-217 set
			// these from strings, empty for a P224 key's OpenSSH ones.
			"tls_public_key": {Filled: []string{
				"id", "algorithm", "public_key_pem", "public_key_openssh",
				"public_key_fingerprint_md5", "public_key_fingerprint_sha256",
			}},
		}, Types: map[string]annotation{
			"tls_private_key": {
				Attrs: map[string]types.Type{
					"algorithm":             types.EnumOf("RSA", "ECDSA", "ED25519"),
					"ecdsa_curve":           types.EnumOf("P224", "P256", "P384", "P521"),
					"private_key_pem":       pem("tls", "PrivateKey"),
					"private_key_pem_pkcs8": pem("tls", "PrivateKey"),
					"public_key_pem":        pem("tls", "PublicKey"),
				},
				Filled: []string{
					"id", "rsa_bits", "ecdsa_curve", "private_key_pem", "private_key_pem_pkcs8",
					"private_key_openssh", "public_key_pem", "public_key_openssh",
					"public_key_fingerprint_md5", "public_key_fingerprint_sha256",
				},
			},
			"tls_cert_request": {
				Attrs: map[string]types.Type{
					"private_key_pem":  pem("tls", "PrivateKey"),
					"cert_request_pem": pem("tls", "CertRequest"),
				},
				Single: []string{"subject"},
				Filled: []string{"id", "cert_request_pem", "key_algorithm"},
			},
			"tls_self_signed_cert": {
				Attrs: map[string]types.Type{
					"private_key_pem": pem("tls", "PrivateKey"),
					"cert_pem":        pem("tls", "SelfSignedCert"),
				},
				Single: []string{"subject"},
				Filled: []string{
					"id", "cert_pem", "key_algorithm", "validity_start_time", "validity_end_time",
					"ready_for_renewal", "early_renewal_hours", "is_ca_certificate",
					"set_subject_key_id", "set_authority_key_id",
				},
			},
			"tls_locally_signed_cert": {
				Attrs: map[string]types.Type{
					"cert_request_pem":   pem("tls", "CertRequest"),
					"ca_private_key_pem": pem("tls", "PrivateKey"),
					"ca_cert_pem":        pem("tls", "SelfSignedCert", "LocallySignedCert"),
					"cert_pem":           pem("tls", "LocallySignedCert"),
				},
				Filled: []string{
					"id", "cert_pem", "ca_key_algorithm", "validity_start_time", "validity_end_time",
					"ready_for_renewal", "early_renewal_hours", "is_ca_certificate", "set_subject_key_id",
				},
			},
		}},
		// Filled: each Create in random v3.9.1 sets these, or the schema
		// gives a Default, and an apply on 2026-11-11 left none of them null.
		{Namespace: "hashicorp", Type: "random"}: {Types: map[string]annotation{
			"random_bytes":    {Filled: []string{"base64", "hex"}},
			"random_id":       {Filled: []string{"id", "b64_std", "b64_url", "dec", "hex"}},
			"random_integer":  {Filled: []string{"id", "result"}},
			"random_password": {Filled: append([]string{"bcrypt_hash"}, randomString...)},
			"random_pet":      {Filled: []string{"id", "length", "separator"}},
			"random_shuffle":  {Filled: []string{"id", "result"}},
			"random_string":   {Filled: randomString},
			"random_uuid":     {Filled: []string{"id", "result"}},
			"random_uuid4":    {Filled: []string{"id", "result"}},
			"random_uuid7":    {Filled: []string{"id", "result"}},
		}},
		// Filled: each Create in local v2.9.1 sets these, or the schema
		// gives a Default, and an apply on 2026-11-11 left none of them null.
		{Namespace: "hashicorp", Type: "local"}: {Data: map[string]annotation{
			"local_file":           localFileData,
			"local_sensitive_file": localFileData,
		}, Types: map[string]annotation{
			"local_file": {Filled: localFile},
			"local_sensitive_file": {
				Secret: []string{
					"content_md5", "content_sha1", "content_sha256", "content_sha512",
					"content_base64sha256", "content_base64sha512",
				},
				Filled: localFile,
			},
		}},
	}
}

// aws annotates hashicorp/aws. The families and suffixes cover the types
// whose references were checked by hand against the 6.70.0 schema. Widen
// them only after checking every slot that would infer the new member:
// TestAWSReferenceCoverage in internal/provider reads the cached schema.
func aws() providerAnnotations {
	lb := map[string]string{
		"load_balancer_arn":                       "aws_lb",
		"default_action.target_group_arn":         "aws_lb_target_group",
		"default_action.forward.target_group.arn": "aws_lb_target_group",
		"certificate_arn":                         "aws_acm_certificate",
	}
	rule := map[string]string{
		"listener_arn":                    "aws_lb_listener",
		"action.target_group_arn":         "aws_lb_target_group",
		"action.forward.target_group.arn": "aws_lb_target_group",
	}
	listenerCert := map[string]string{"listener_arn": "aws_lb_listener", "certificate_arn": "aws_acm_certificate"}
	attachment := map[string]string{"target_group_arn": "aws_lb_target_group"}
	policyArn := map[string]string{"policy_arn": "aws_iam_policy"}
	// Listeners, their rules, and certificates have ARNs for ids and no
	// name attribute.
	listenerLabel := &Label{Parent: "load_balancer_arn", Format: "{protocol}_{port}"}
	ruleLabel := &Label{Parent: "listener_arn", Format: "priority_{priority}"}
	pa := providerAnnotations{
		Families: []family{
			{Kind: types.IdRef, Wires: []string{"aws_vpc", "aws_default_vpc"}},
			{Kind: types.IdRef, Wires: []string{"aws_subnet", "aws_default_subnet"}},
			{Kind: types.IdRef, Wires: []string{"aws_security_group", "aws_default_security_group"}},
			{Kind: types.IdRef, Wires: []string{"aws_route_table", "aws_default_route_table"}},
			{Kind: types.IdRef, Wires: []string{"aws_network_acl", "aws_default_network_acl"}},
			{Kind: types.IdRef, Wires: []string{"aws_vpc_dhcp_options", "aws_default_vpc_dhcp_options"}},
			{Kind: types.IdRef, Wires: []string{"aws_vpc_peering_connection", "aws_vpc_peering_connection_accepter"}},
			{Kind: types.IdRef, Wires: []string{"aws_internet_gateway"}},
			{Kind: types.IdRef, Wires: []string{"aws_nat_gateway"}},
			{Kind: types.IdRef, Wires: []string{"aws_vpn_gateway"}},
			{Kind: types.IdRef, Wires: []string{"aws_customer_gateway"}},
			{Kind: types.IdRef, Wires: []string{"aws_vpc_endpoint"}},
			{Kind: types.IdRef, Wires: []string{"aws_ec2_transit_gateway"}},
			{Kind: types.IdRef, Wires: []string{"aws_ec2_transit_gateway_route_table"}},
			{Kind: types.IdRef, Wires: []string{"aws_network_interface"}},
			{Kind: types.IdRef, Wires: []string{"aws_eip"}},
			{Kind: types.IdRef, Wires: []string{"aws_instance"}},
			{Kind: types.IdRef, Wires: []string{"aws_launch_template"}},
			{Kind: types.IdRef, Wires: []string{"aws_ebs_volume"}},
			{Kind: types.IdRef, Wires: []string{"aws_efs_file_system"}},
			{Kind: types.IdRef, Wires: []string{"aws_route53_zone"}},
			// These types' ids are their ARNs.
			{Kind: types.ArnRef, Wires: []string{"aws_lb", "aws_alb"}},
			{Kind: types.ArnRef, Wires: []string{"aws_lb_target_group", "aws_alb_target_group"}},
			{Kind: types.ArnRef, Wires: []string{"aws_lb_listener", "aws_alb_listener"}},
			{Kind: types.ArnRef, Wires: []string{"aws_acm_certificate"}},
			{Kind: types.ArnRef, Wires: []string{"aws_iam_policy"}},
			{Kind: types.ArnRef, Wires: []string{"aws_iam_service_linked_role"}},
			{Kind: types.ArnRef, Wires: []string{"aws_acmpca_certificate_authority"}},
			{Kind: types.ArnRef, Wires: []string{"aws_sns_topic"}},
			{Kind: types.ArnRef, Wires: []string{"aws_secretsmanager_secret"}},
			// These types' ids are their names.
			{Kind: types.NameRef, Wires: []string{"aws_iam_role"}, Arn: true},
			{Kind: types.NameRef, Wires: []string{"aws_iam_instance_profile"}},
			{Kind: types.NameRef, Wires: []string{"aws_cloudwatch_log_group"}, Arn: true},
			{Kind: types.NameRef, Wires: []string{"aws_lambda_function"}, Arn: true},
			{Kind: types.NameRef, Wires: []string{"aws_s3_bucket"}, Arn: true},
			// A KMS key slot named <x>_id takes the key's id, its ARN, or an
			// alias, so only <x>_arn slots are typed.
			{
				Kind:  types.IdRef,
				Wires: []string{"aws_kms_key", "aws_kms_replica_key", "aws_kms_external_key", "aws_kms_replica_external_key"},
				Arn:   true, IdTakesArn: true,
			},
		},
		// Each entry was checked against every slot it types in the 6.70.0
		// schema. The service's own type of the same name wins: certificate_arn
		// in aws_dms_endpoint is a DMS certificate, so it stays plain.
		Suffixes: map[string][]string{
			"role_arn":                  {"aws_iam_role", "aws_iam_service_linked_role"},
			"kms_key_arn":               {"aws_kms_key"},
			"load_balancer_arn":         {"aws_lb"},
			"listener_arn":              {"aws_lb_listener"},
			"target_group_arn":          {"aws_lb_target_group"},
			"certificate_arn":           {"aws_acm_certificate"},
			"certificate_authority_arn": {"aws_acmpca_certificate_authority"},
			"policy_arn":                {"aws_iam_policy"},
			"topic_arn":                 {"aws_sns_topic"},
			"secret_arn":                {"aws_secretsmanager_secret"},
			"log_group_arn":             {"aws_cloudwatch_log_group"},
			"function_arn":              {"aws_lambda_function"},
			"lambda_arn":                {"aws_lambda_function"},
			"bucket_arn":                {"aws_s3_bucket"},
			"table_bucket_arn":          nil, // an S3 Tables bucket
			"vector_bucket_arn":         nil, // an S3 Vectors bucket
			"security_group_id":         {"aws_security_group"},
			"subnet_id":                 {"aws_subnet"},
			"secondary_subnet_id":       nil, // an EC2 secondary subnet
			"vpc_id":                    {"aws_vpc"},
		},
		Names: map[string]string{
			"vpc_security_group_ids": "aws_security_group",
			// security group IDs, or names in a default VPC
			"security_groups":     "aws_security_group",
			"vpc_zone_identifier": "aws_subnet",
		},
		Data: map[string]annotation{
			"aws_vpcs":            {Refs: map[string]string{"ids": "aws_vpc"}},
			"aws_subnets":         {Refs: map[string]string{"ids": "aws_subnet"}},
			"aws_security_groups": {Refs: map[string]string{"ids": "aws_security_group"}},
		},
		Types: map[string]annotation{
			"aws_instance": {Refs: map[string]string{
				"iam_instance_profile": "aws_iam_instance_profile",
				"launch_template.id":   "aws_launch_template",
			}},
			"aws_launch_template": {
				// AWS 6.70.0 ec2/ec2_launch_template.go:1177-1190 never reads
				// the switch; Update at 1255-1270 sets default to latest when true.
				// CustomizeDiff at 1083-1093 makes default_version computed.
				// TestImportAWSSettingsOffline proves import and read behavior.
				SettingsVersion: "6.70.0",
				Settings:        &SettingsRule{Attribute: "update_default_version", Consequence: "default_version", Matches: "latest_version"},
				Refs:            map[string]string{"iam_instance_profile.name": "aws_iam_instance_profile"},
			},
			"aws_autoscaling_group": {
				// AWS 6.70.0 autoscaling/group.go:49 excludes
				// wait_for_capacity_timeout from import, and
				// resourceGroupFlatten never sets it.
				ReadDefaultsVersion: "6.70.0",
				ReadDefaults: map[string]string{
					"wait_for_capacity_timeout": `"10m"`,
				},
				Refs: map[string]string{
					"target_group_arns":  "aws_lb_target_group",
					"launch_template.id": "aws_launch_template",
				},
			},
			"aws_lb":                          {Refs: map[string]string{"subnets": "aws_subnet"}},
			"aws_alb":                         {Refs: map[string]string{"subnets": "aws_subnet"}},
			"aws_elb":                         {Refs: map[string]string{"subnets": "aws_subnet"}},
			"aws_autoscaling_attachment":      {Refs: map[string]string{"lb_target_group_arn": "aws_lb_target_group"}},
			"aws_lb_listener":                 {Refs: lb, Label: listenerLabel, ImportDisabled: map[string]DisabledWhen{"mutual_authentication": {Field: "mode", Value: "off"}, "default_action.forward.stickiness": {Field: "enabled", Value: "false"}}},
			"aws_alb_listener":                {Refs: lb, Label: listenerLabel},
			"aws_lb_listener_rule":            {SettlingReadVersion: "6.70.0", Refs: rule, Label: ruleLabel, ImportDisabled: map[string]DisabledWhen{"action.forward.stickiness": {Field: "enabled", Value: "false"}}},
			"aws_alb_listener_rule":           {Refs: rule, Label: ruleLabel},
			"aws_acm_certificate":             {Label: &Label{Format: "{domain_name}"}},
			"aws_lb_listener_certificate":     {Refs: listenerCert},
			"aws_alb_listener_certificate":    {Refs: listenerCert},
			"aws_lb_target_group_attachment":  {Refs: attachment},
			"aws_alb_target_group_attachment": {Refs: attachment},
			"aws_iam_instance_profile":        {Refs: map[string]string{"role": "aws_iam_role"}},
			"aws_iam_role_policy":             {Refs: map[string]string{"role": "aws_iam_role"}},
			"aws_iam_role_policy_attachment":  {Refs: map[string]string{"role": "aws_iam_role", "policy_arn": "aws_iam_policy"}},
			"aws_iam_policy_attachment":       {Refs: map[string]string{"roles": "aws_iam_role", "policy_arn": "aws_iam_policy"}},
			"aws_iam_user_policy_attachment":  {Refs: policyArn},
			"aws_iam_group_policy_attachment": {Refs: policyArn},
			"aws_volume_attachment":           {Refs: map[string]string{"volume_id": "aws_ebs_volume"}},
			"aws_nat_gateway":                 {Refs: map[string]string{"allocation_id": "aws_eip"}},
			"aws_eip_association":             {Refs: map[string]string{"allocation_id": "aws_eip"}},
			// An alias names the hosted zone of its target, such as a load
			// balancer's zone_id, not a Route 53 zone.
			"aws_route53_record": {Plain: []string{"alias.zone_id"}},
			// These name an RDS instance, or an EC2 or RDS instance.
			"aws_quicksight_data_source": {Plain: []string{"parameters.rds.instance_id"}},
			"aws_budgets_budget_action":  {Plain: []string{"definition.ssm_action_definition.instance_ids"}},
			// This takes the ARN of a CA certificate imported into ACM, not of
			// a private CA.
			"aws_networkfirewall_tls_inspection_configuration": {Plain: []string{"tls_inspection_configuration.server_certificate_configuration.certificate_authority_arn"}},
			// This is a VPC Lattice target group, not a load balancer's.
			"aws_ecs_service": {
				Refs:  map[string]string{"load_balancer.target_group_arn": "aws_lb_target_group"},
				Plain: []string{"vpc_lattice_configurations.target_group_arn"},
			},
			// This is an EKS access policy, not an IAM policy.
			"aws_eks_access_policy_association": {Plain: []string{"policy_arn"}},
			// This takes an S3 bucket or an S3 on Outposts access point.
			"aws_datasync_location_s3": {Plain: []string{"s3_bucket_arn"}},
			// This takes an ACM certificate or an IAM server certificate.
			"aws_appsync_domain_name": {Plain: []string{"certificate_arn"}},
			// These take the gateway's IP address.
			"aws_storagegateway_cached_iscsi_volume": {Plain: []string{"network_interface_id"}},
			"aws_storagegateway_stored_iscsi_volume": {Plain: []string{"network_interface_id"}},

			"aws_iam_role": {ImportOwners: map[string]InlineOwner{"inline_policy": {Computed: true, Wire: "aws_iam_role_policy", ParentField: "name", OwnerField: "role", InlineKey: "name", OwnerKey: "name"}}},
			"aws_s3_bucket": {ImportOwners: map[string]InlineOwner{
				"grant":                                {Computed: true, Wire: "aws_s3_bucket_acl", ParentField: "bucket", OwnerField: "bucket"},
				"lifecycle_rule":                       {Computed: true, Wire: "aws_s3_bucket_lifecycle_configuration", ParentField: "bucket", OwnerField: "bucket"},
				"server_side_encryption_configuration": {Computed: true, Wire: "aws_s3_bucket_server_side_encryption_configuration", ParentField: "bucket", OwnerField: "bucket"},
				"versioning":                           {Computed: true, Wire: "aws_s3_bucket_versioning", ParentField: "bucket", OwnerField: "bucket"},
			}},
		},
	}
	// Filled: writ import links a reference to a family member's id, and to
	// its arn when the arn is typed, so these must read as non-null. Each
	// type is an SDKv2 resource in 6.70.0. SDKv2 v2.40.1 returns no object
	// when a create leaves the id empty, so a created object has an id. Each
	// arn type's Read sets the arn, and SDKv2 stores a d.Set of nil as "".
	for _, wire := range []string{
		"aws_vpc", "aws_default_vpc", "aws_subnet", "aws_default_subnet",
		"aws_security_group", "aws_default_security_group", "aws_route_table", "aws_default_route_table",
		"aws_network_acl", "aws_default_network_acl", "aws_vpc_dhcp_options", "aws_default_vpc_dhcp_options",
		"aws_vpc_peering_connection", "aws_vpc_peering_connection_accepter", "aws_internet_gateway",
		"aws_nat_gateway", "aws_vpn_gateway", "aws_customer_gateway", "aws_vpc_endpoint",
		"aws_ec2_transit_gateway", "aws_ec2_transit_gateway_route_table", "aws_network_interface", "aws_eip",
		"aws_instance", "aws_launch_template", "aws_ebs_volume", "aws_efs_file_system", "aws_route53_zone",
		"aws_iam_instance_profile",
	} {
		pa.fill(wire, "id")
	}
	for _, wire := range []string{
		"aws_lb", "aws_alb", "aws_lb_target_group", "aws_alb_target_group", "aws_lb_listener", "aws_alb_listener",
		"aws_acm_certificate", "aws_iam_policy", "aws_iam_service_linked_role", "aws_acmpca_certificate_authority",
		"aws_sns_topic", "aws_secretsmanager_secret", "aws_iam_role", "aws_cloudwatch_log_group",
		"aws_lambda_function", "aws_s3_bucket", "aws_kms_key", "aws_kms_replica_key", "aws_kms_external_key",
		"aws_kms_replica_external_key",
	} {
		pa.fill(wire, "id", "arn")
	}
	// Filled outputs of common types. Each type is an SDKv2 resource in
	// 6.70.0. The line names, under internal/service, where Create sets the
	// id and where Read sets each attribute with d.Set on every path that
	// returns no error. Each Create ends in Read, or in an Update that does,
	// and SDKv2 v2.40.1 stores a d.Set of nil as "" or 0, never as null
	// (helper/schema/field_writer_map.go:246-249). A create that returns no
	// object is an error, so a recorded object always has these.
	for wire, attrs := range map[string][]string{
		// ec2/vpc_.go:735-740, and 305-321 where both branches set the ids
		"aws_vpc": {"cidr_block", "default_route_table_id", "main_route_table_id", "default_security_group_id", "owner_id"},
		// ec2/vpc_subnet.go:795-807
		"aws_subnet": {"arn", "availability_zone", "availability_zone_id", "cidr_block", "owner_id"},
		// ec2/vpc_security_group.go:302-308
		"aws_security_group": {"arn", "name", "owner_id", "vpc_id"},
		// ec2/ec2_instance.go:3219, 3268-3271, 3446. An address the
		// instance does not have is "".
		"aws_instance": {"arn", "instance_state", "private_dns", "private_ip", "public_dns", "public_ip"},
		// elbv2/load_balancer.go:626-648, Create waits for active at 499
		"aws_lb":  {"arn_suffix", "dns_name", "zone_id", "vpc_id"},
		"aws_alb": {"arn_suffix", "dns_name", "zone_id", "vpc_id"},
		// elbv2/target_group.go:629-636
		"aws_lb_target_group":  {"arn_suffix", "name"},
		"aws_alb_target_group": {"arn_suffix", "name"},
		// route53/zone.go:222-259. Read indexes name_servers[0], so a
		// read that returns no error has a non-empty list.
		"aws_route53_zone": {"arn", "zone_id", "name_servers"},
		// eks/cluster.go:799 and 1162-1219, Create waits for ACTIVE at 801.
		// certificate_authority is [] when the API sends none (2111-2121).
		"aws_eks_cluster": {"id", "arn", "certificate_authority", "endpoint", "platform_version", "status", "version"},
		// ecs/cluster.go:214 and 473
		"aws_ecs_cluster": {"id", "arn"},
		// ecs/service.go:1580-1581 and 3866
		"aws_ecs_service": {"id", "arn"},
		// ecs/task_definition.go:698 and 1322-1338
		"aws_ecs_task_definition": {"id", "arn", "arn_without_revision", "revision"},
		// ecr/repository.go:188 and 231-238
		"aws_ecr_repository": {"id", "arn", "registry_id", "repository_url"},
		// rds/instance.go:1915, 1986-2094. The id is the DbiResourceId.
		// Read sets address, endpoint, and hosted_zone_id only when the API
		// sends an endpoint (2106-2111), so they are not Filled.
		"aws_db_instance": {"id", "arn", "identifier", "port", "resource_id"},
		// rds/cluster.go:1462 and 2385-2467
		"aws_rds_cluster": {"id", "arn", "cluster_resource_id", "endpoint", "hosted_zone_id", "port", "reader_endpoint"},
		// iam/role.go:746-758
		"aws_iam_role": {"name", "unique_id"},
		// iam/policy.go:320-325
		"aws_iam_policy": {"name", "policy_id"},
		// lambda/function.go:2074 and 2126-2142, where both branches set
		// the qualified values
		"aws_lambda_function": {"invoke_arn", "qualified_arn", "qualified_invoke_arn", "version"},
		// s3/bucket.go:1757-1770
		"aws_s3_bucket": {"bucket", "bucket_domain_name", "bucket_regional_domain_name"},
		// kms/key.go:762
		"aws_kms_key": {"key_id"},
		// sns/topic.go:534-539
		"aws_sns_topic": {"name"},
		// sqs/queue.go:262, 309-330, and 314 through attrmap, which sets a
		// missing attribute to nil (internal/attrmap/attrmap.go:90-94)
		"aws_sqs_queue": {"id", "arn", "name", "url"},
		// logs/group.go:318
		"aws_cloudwatch_log_group": {"name"},
	} {
		pa.fill(wire, attrs...)
	}
	// Filled for lookups: the read in 6.70.0 named on each line sets these
	// on every path that returns no error. An SDKv2 data source that sets
	// no id returns every attribute null (SDKv2 v2.40.1
	// terraform/state.go:1376), so each line also names its SetId. The
	// S3 bucket read sets website_endpoint only for a bucket with a
	// website (s3/bucket_data_source.go:106-109), so it is not Filled.
	for wire, attrs := range map[string][]string{
		// ec2/ec2_ami_data_source.go:310-330
		"aws_ami": {"id", "arn", "architecture", "creation_date", "image_id", "name", "owner_id"},
		// iam/policy_document_data_source.go:321-332
		"aws_iam_policy_document": {"id", "json", "minified_json"},
		// meta/partition_data_source.go:56-59, framework, StringValueToFrameworkLegacy
		"aws_partition": {"id", "dns_suffix", "partition", "reverse_dns_prefix"},
		// meta/region_data_source.go:127-131, framework, StringValueToFrameworkLegacy
		"aws_region": {"id", "name", "region", "description", "endpoint"},
		// s3/bucket_data_source.go:92-100
		"aws_s3_bucket": {"id", "arn", "bucket_domain_name", "bucket_region", "bucket_regional_domain_name"},
		// ssm/parameter_data_source.go:73-82. value is sensitive, so a lookup cannot read it.
		"aws_ssm_parameter": {"id", "arn", "type", "version"},
		// ec2/vpc_subnet_data_source.go:202-243
		"aws_subnet": {"id", "arn", "availability_zone", "cidr_block", "owner_id", "vpc_id"},
		// ec2/vpc_subnets_data_source.go:78-79, a d.Set of a slice is a list
		"aws_subnets": {"id", "ids"},
		// ec2/vpc_data_source.go:208-215 and 235-240, where both branches set main_route_table_id
		"aws_vpc": {"id", "arn", "cidr_block", "owner_id", "main_route_table_id"},
	} {
		pa.fillData(wire, attrs...)
	}
	return pa
}

// fill adds attrs to the Filled list of the resource type wire.
func (pa providerAnnotations) fill(wire string, attrs ...string) {
	a := pa.Types[wire]
	a.Filled = append(a.Filled, attrs...)
	pa.Types[wire] = a
}

// fillData adds attrs to the Filled list of the data source wire.
func (pa providerAnnotations) fillData(wire string, attrs ...string) {
	a := pa.Data[wire]
	a.Filled = append(a.Filled, attrs...)
	pa.Data[wire] = a
}

func pem(provider string, targets ...string) types.Type {
	names := make([]types.Name, len(targets))
	for i, t := range targets {
		names[i] = types.Name{Provider: provider, Type: t}
	}
	return types.RefTo(types.PemRef, names...)
}
