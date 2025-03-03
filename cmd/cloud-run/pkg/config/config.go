// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

type DeployOptions struct {
	Service string
	Image   string
	Source  string

	// Environment variables configuration
	EnvVars       map[string]string // Environment variables to set
	EnvVarsFile   string            // Path to YAML file with environment variables
	RemoveEnvVars []string          // Environment variables to remove
	UpdateEnvVars map[string]string // Environment variables to update
	ClearEnvVars  bool              // Whether to clear all environment variables

	// Secrets configuration
	Secrets       map[string]string // Secrets to set (key=SECRET_NAME:VERSION)
	RemoveSecrets []string          // Secrets to remove
	UpdateSecrets map[string]string // Secrets to update
	ClearSecrets  bool              // Whether to clear all secrets

	// Access and traffic configuration
	AllowUnauthenticated bool   // Whether to allow unauthenticated access (if false, --no-allow-unauthenticated)
	Ingress              string // Ingress setting: all, internal, or internal-and-cloud-load-balancing
	DefaultURL           bool   // Whether to use the default URL (if false, --no-default-url)

	// VPC connectivity configuration
	VpcConnector  string // VPC connector to use, empty means no VPC connector
	VpcNetwork    string // VPC network name, typically "default"
	VpcSubnetwork string // VPC subnetwork name, typically "default"
	VpcEgress     string // VPC egress setting: "private-ranges-only" or "all-traffic"
}
