package tfcode

import (
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

var awsConfig = schema.Block{
	Attributes: []schema.Attribute{
		{Name: "region", Type: types.TString, Optional: true},
		{Name: "max_retries", Type: types.TInt, Optional: true},
		{Name: "skip_credentials_validation", Type: types.TBool, Optional: true},
		{Name: "secret_key", Type: types.TString, Optional: true, Sensitive: true},
		{Name: "allowed_account_ids", Type: types.ListOf(types.TString), Optional: true},
	},
	Blocks: []schema.NestedBlock{
		{Name: "endpoints", Nesting: schema.NestList, Block: schema.Block{Attributes: []schema.Attribute{{Name: "ec2", Type: types.TString, Optional: true}}}},
		{Name: "assume_role", Nesting: schema.NestSingle, Block: schema.Block{Attributes: []schema.Attribute{{Name: "role_arn", Type: types.TString, Optional: true}}}},
	},
}

func TestRead(t *testing.T) {
	st, err := Read(map[string][]byte{
		"main.tf": []byte(`terraform {
  required_providers {
    aws  = { source = "hashicorp/aws", version = "6.70.0" }
    mine = { source = "example.com/team/mine" }
  }
}

provider "aws" {
  region                      = "eu-west-1"
  max_retries                 = 3
  skip_credentials_validation = true
  secret_key                  = "mock-secret-key"
  allowed_account_ids         = ["123456789012"]
  endpoints {
    ec2 = "http://127.0.0.1:9"
  }
  assume_role {
    role_arn = "arn:aws:iam::123456789012:role/read"
  }
}

provider "aws" {
  alias  = "west"
  region = var.region
}

variable "region" {}

resource "aws_vpc" "main" {}
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Source("mine").String(); got != "example.com/team/mine" {
		t.Errorf("source of mine = %s", got)
	}
	if got := st.Source("random").String(); got != "hashicorp/random" {
		t.Errorf("implied source of random = %s", got)
	}
	if !st.Resources["aws_vpc"]["main"] || !st.Variables["region"] {
		t.Errorf("declared names = %v, %v", st.Resources, st.Variables)
	}
	cfg, err := st.Providers[ProviderRef{Name: "aws"}].Config(awsConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := value.ObjectVal(map[string]value.Value{
		"region": value.StringVal("eu-west-1"), "max_retries": value.IntVal(3), "skip_credentials_validation": value.BoolVal(true),
		"secret_key": value.StringVal("mock-secret-key").Secret(), "allowed_account_ids": value.ListVal([]value.Value{value.StringVal("123456789012")}),
		"endpoints":   value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{"ec2": value.StringVal("http://127.0.0.1:9")})}),
		"assume_role": value.ObjectVal(map[string]value.Value{"role_arn": value.StringVal("arn:aws:iam::123456789012:role/read")}),
	})
	if !value.Equal(cfg, want) || !cfg.Get("secret_key").IsSecret() {
		t.Errorf("config =\n%s\nwant\n%s", cfg, want)
	}
	west := st.Providers[ProviderRef{Name: "aws", Alias: "west"}]
	if west == nil || west.Ref.String() != "aws.west" {
		t.Fatalf("providers = %v", st.Providers)
	}
	if _, err := west.Config(awsConfig); err == nil || !strings.Contains(err.Error(), "region is not a literal value") {
		t.Errorf("a variable in a provider block: %v", err)
	}
	if cfg, err := (*Provider)(nil).Config(awsConfig); err != nil || cfg.Len() != 0 {
		t.Errorf("no provider block: %s, %v; want an empty configuration", cfg, err)
	}
}

func TestReadRefuses(t *testing.T) {
	for name, code := range map[string]string{
		"an argument the provider lacks": "provider \"aws\" {\n  colour = \"blue\"\n}\n",
		"a block the provider lacks":     "provider \"aws\" {\n  colour {}\n}\n",
		"a string for a number":          "provider \"aws\" {\n  max_retries = \"many\"\n}\n",
		"a fraction for a whole number":  "provider \"aws\" {\n  max_retries = 1.5\n}\n",
	} {
		st, err := Read(map[string][]byte{"main.tf": []byte(code)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := st.Providers[ProviderRef{Name: "aws"}].Config(awsConfig); err == nil {
			t.Errorf("%s: Config accepted it", name)
		}
	}
	if _, err := Read(map[string][]byte{"main.tf": []byte("provider \"aws\" {}\nprovider \"aws\" {}\n")}); err == nil {
		t.Error("Read accepted two provider blocks without an alias")
	}
}
