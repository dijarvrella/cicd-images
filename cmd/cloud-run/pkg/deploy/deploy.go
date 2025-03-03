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

package deploy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dijarvrella/cicd-images/cmd/cloud-run/pkg/config"
	"github.com/dijarvrella/cicd-images/cmd/cloud-run/pkg/utils"

	"google.golang.org/api/googleapi"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/api/run/v2"
)

// VpcConfig represents the VPC connectivity configuration
type VpcConfig struct {
	Network    string `json:"network"`
	Subnetwork string `json:"subnetwork"`
	// Egress can be "all-traffic" or "private-ranges-only"
	Egress string `json:"egress"`
}

// CreateOrUpdateService deploys a service to Cloud run. If the service
// doesn't exist, it creates a new one. If the service exists, it updates the
// existing service with the config.DeployOptions.
func CreateOrUpdateService(runAPIClient *runv1.APIService, projectID, region string, opts config.DeployOptions) error {
	parent := fmt.Sprintf("projects/%s/locations/%s", projectID, region)
	resourceName := fmt.Sprintf("%s/services/%s", parent, opts.Service)
	log.Printf("Deploying container to Cloud Run service [%s] in project [%s] region [%s]\n", opts.Service, projectID, region)

	existingService, err := runAPIClient.Projects.Locations.Services.Get(resourceName).Do()
	if err != nil {
		gErr, ok := err.(*googleapi.Error)
		if !ok || gErr.Code != http.StatusNotFound {
			return err
		}
		serviceDefinition := buildServiceDefinition(projectID, opts)
		log.Printf("Creating a new serivce %s\n", opts.Service)
		createCall := runAPIClient.Projects.Locations.Services.Create(parent, &serviceDefinition)
		_, err = createCall.Do()
		if err != nil {
			return err
		}
	} else {
		log.Printf("Replacing the existing serivce %s\n", opts.Service)
		updateWithOptions(existingService, opts)
		replaceCall := runAPIClient.Projects.Locations.Services.ReplaceService(resourceName, existingService)
		_, err = replaceCall.Do()
		if err != nil {
			return err
		}
	}

	return nil
}

// WaitForServiceReady waits for a Cloud Run service to reach a ready state
// by polling its status.
func WaitForServiceReady(ctx context.Context, runAPIClient *runv1.APIService, projectID, region, service string) error {
	resourceName := fmt.Sprintf("projects/%s/locations/%s/services/%s", projectID, region, service)
	if err := utils.PollWithInterval(ctx, time.Minute*2, time.Second, func() (bool, error) {
		runService, err := runAPIClient.Projects.Locations.Services.Get(resourceName).Do()
		if err != nil {
			return false, err
		}

		// Clients polling for completed reconciliation should poll until
		// observedGeneration = metadata.generation and the Ready condition's
		// status is True or False
		// see details in: https://github.com/googleapis/google-api-go-client/blob/v0.169.0/run/v1/run-gen.go#L961-L967
		if runService.Status.ObservedGeneration != runService.Metadata.Generation {
			return false, nil
		}
		for _, c := range runService.Status.Conditions {
			if c.Type == "Ready" {
				log.Println(c.Message)
				if c.Status == "True" {
					return true, nil
				}
				if c.Status == "False" {
					return false, fmt.Errorf("failed to deploy the latest revision of the service %s", service)
				}
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	runService, err := runAPIClient.Projects.Locations.Services.Get(resourceName).Do()
	log.Printf("Service [%s] with revision [%s] is deployed successfully, serving %d percent of traffic.\n",
		service, runService.Status.LatestReadyRevisionName, runService.Status.Traffic[0].Percent)
	log.Printf("Service URL: %s \n ", runService.Status.Url)
	if err != nil {
		return err
	}
	return nil
}

// parseSecretReference parses a secret reference in the format projects/PROJECT_ID/secrets/SECRET_NAME/versions/VERSION
// or the simpler format SECRET_NAME:VERSION
// and returns the secret name and version
func parseSecretReference(secretRef string) (secretName, version string) {
	log.Printf("Parsing secret reference: %s", secretRef)

	// Remove any whitespace
	secretRef = strings.TrimSpace(secretRef)

	// Handle empty input
	if secretRef == "" {
		return "", "latest"
	}

	// Check if it's in the format projects/PROJECT_ID/secrets/SECRET_NAME/versions/VERSION
	if strings.Contains(secretRef, "projects/") && strings.Contains(secretRef, "/secrets/") {
		parts := strings.Split(secretRef, "/secrets/")
		if len(parts) == 2 {
			// Get everything up to the /versions/ part if it exists
			secretVersionParts := strings.Split(parts[1], "/versions/")
			secretName = secretVersionParts[0]

			// Handle version
			if len(secretVersionParts) >= 2 {
				version = secretVersionParts[1]
			} else {
				version = "latest" // Default to latest if not specified
			}

			log.Printf("Parsed from project format - Secret name: %s, Version: %s", secretName, version)
			return
		}
	}

	// Fall back to simpler parsing (SECRET_NAME:VERSION)
	parts := strings.Split(secretRef, ":")
	if len(parts) >= 1 {
		secretName = parts[0]
		if len(parts) >= 2 {
			version = parts[1]
		} else {
			version = "latest" // Default to latest if not specified
		}
	}

	log.Printf("Parsed from simple format - Secret name: %s, Version: %s", secretName, version)
	return
}

// updateWithOptions updates the image and configuration of an existing Cloud Run Service object
// based on the provided config.DeployOptions.
func updateWithOptions(service *runv1.Service, opts config.DeployOptions) {
	service.Spec.Template.Spec.Containers[0].Image = opts.Image

	// Handle environment variables
	container := service.Spec.Template.Spec.Containers[0]
	if opts.ClearEnvVars {
		container.Env = nil
	} else if opts.EnvVars != nil && len(opts.EnvVars) > 0 {
		container.Env = make([]*runv1.EnvVar, 0, len(opts.EnvVars))
		for k, v := range opts.EnvVars {
			container.Env = append(container.Env, &runv1.EnvVar{
				Name:  k,
				Value: v,
			})
		}
	} else {
		if opts.RemoveEnvVars != nil && len(opts.RemoveEnvVars) > 0 && container.Env != nil {
			// Remove specified environment variables
			newEnv := make([]*runv1.EnvVar, 0, len(container.Env))
			for _, env := range container.Env {
				shouldKeep := true
				for _, remove := range opts.RemoveEnvVars {
					if env.Name == remove {
						shouldKeep = false
						break
					}
				}
				if shouldKeep {
					newEnv = append(newEnv, env)
				}
			}
			container.Env = newEnv
		}
		if opts.UpdateEnvVars != nil && len(opts.UpdateEnvVars) > 0 {
			// Update or add new environment variables
			if container.Env == nil {
				container.Env = make([]*runv1.EnvVar, 0, len(opts.UpdateEnvVars))
			}
			for k, v := range opts.UpdateEnvVars {
				found := false
				for _, env := range container.Env {
					if env.Name == k {
						env.Value = v
						found = true
						break
					}
				}
				if !found {
					container.Env = append(container.Env, &runv1.EnvVar{
						Name:  k,
						Value: v,
					})
				}
			}
		}
	}

	// Handle secrets
	if opts.ClearSecrets {
		// Clear secret environment variables
		if container.Env != nil {
			// Filter out any secret-referenced env vars
			newEnv := make([]*runv1.EnvVar, 0, len(container.Env))
			for _, env := range container.Env {
				if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
					newEnv = append(newEnv, env)
				}
			}
			container.Env = newEnv
		}

		// Clear volume mounts and volumes
		container.VolumeMounts = nil
		service.Spec.Template.Spec.Volumes = nil
	} else if opts.Secrets != nil && len(opts.Secrets) > 0 {
		// Clear existing secret-referenced env vars
		if container.Env != nil {
			newEnv := make([]*runv1.EnvVar, 0, len(container.Env))
			for _, env := range container.Env {
				if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
					newEnv = append(newEnv, env)
				}
			}
			container.Env = newEnv
		} else {
			container.Env = make([]*runv1.EnvVar, 0)
		}

		// Clear existing volume mounts and volumes
		container.VolumeMounts = make([]*runv1.VolumeMount, 0)
		service.Spec.Template.Spec.Volumes = make([]*runv1.Volume, 0)

		// Add new secrets
		for k, v := range opts.Secrets {
			if k[0] == '/' {
				// Mount secret as volume
				mountPath := k
				secretName, version := parseSecretReference(v)

				// Only proceed if we have a valid secretName
				if secretName != "" {
					container.VolumeMounts = append(container.VolumeMounts, &runv1.VolumeMount{
						Name:      secretName,
						MountPath: mountPath,
					})

					service.Spec.Template.Spec.Volumes = append(service.Spec.Template.Spec.Volumes, &runv1.Volume{
						Name: secretName,
						Secret: &runv1.SecretVolumeSource{
							SecretName: secretName,
							Items: []*runv1.KeyToPath{{
								Key:  version,
								Path: filepath.Base(mountPath),
							}},
						},
					})
				}
			} else {
				// Set as environment variable using ValueFrom
				secretName, version := parseSecretReference(v)

				// Only proceed if we have a valid secretName
				if secretName != "" {
					log.Printf("Adding secret env var %s with secret %s and version %s", k, secretName, version)

					// Create a properly initialized SecretKeySelector with all required fields
					secretKeyRef := &runv1.SecretKeySelector{
						Key:  version,
						Name: secretName,
						LocalObjectReference: &runv1.LocalObjectReference{
							Name: secretName,
						},
					}

					// Use direct struct initialization to avoid null JSON fields
					envVar := &runv1.EnvVar{
						Name:  k,
						Value: "",
						ValueFrom: &runv1.EnvVarSource{
							SecretKeyRef: secretKeyRef,
						},
					}

					// Add to environment variables
					container.Env = append(container.Env, envVar)

					// Log the resulting structure to verify it's set correctly
					log.Printf("Secret reference set: Name=%s, Key=%s",
						secretName, version)
				}
			}
		}
	} else {
		if opts.RemoveSecrets != nil && len(opts.RemoveSecrets) > 0 {
			// Remove specified secrets from environment variables
			if container.Env != nil {
				newEnv := make([]*runv1.EnvVar, 0, len(container.Env))
				for _, env := range container.Env {
					shouldKeep := true
					if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
						secretName := env.ValueFrom.SecretKeyRef.LocalObjectReference.Name
						for _, remove := range opts.RemoveSecrets {
							if secretName == remove {
								shouldKeep = false
								break
							}
						}
					}
					if shouldKeep {
						newEnv = append(newEnv, env)
					}
				}
				container.Env = newEnv
			}

			// Remove specified secrets from volume mounts and volumes
			if container.VolumeMounts != nil {
				newVolumeMounts := make([]*runv1.VolumeMount, 0, len(container.VolumeMounts))
				for _, mount := range container.VolumeMounts {
					shouldKeep := true
					for _, remove := range opts.RemoveSecrets {
						if mount.Name == remove {
							shouldKeep = false
							break
						}
					}
					if shouldKeep {
						newVolumeMounts = append(newVolumeMounts, mount)
					}
				}
				container.VolumeMounts = newVolumeMounts
			}

			if service.Spec.Template.Spec.Volumes != nil {
				newVolumes := make([]*runv1.Volume, 0, len(service.Spec.Template.Spec.Volumes))
				for _, vol := range service.Spec.Template.Spec.Volumes {
					shouldKeep := true
					for _, remove := range opts.RemoveSecrets {
						if vol.Name == remove {
							shouldKeep = false
							break
						}
					}
					if shouldKeep {
						newVolumes = append(newVolumes, vol)
					}
				}
				service.Spec.Template.Spec.Volumes = newVolumes
			}
		}

		if opts.UpdateSecrets != nil && len(opts.UpdateSecrets) > 0 {
			// Update or add new secrets
			for k, v := range opts.UpdateSecrets {
				if k[0] == '/' {
					// Mount secret as volume
					mountPath := k
					secretName, version := parseSecretReference(v)

					// Only proceed if we have a valid secretName
					if secretName != "" {
						// Update existing volume mount or add new one
						found := false
						if container.VolumeMounts != nil {
							for _, mount := range container.VolumeMounts {
								if mount.MountPath == mountPath {
									mount.Name = secretName
									found = true
									break
								}
							}
						}
						if !found {
							if container.VolumeMounts == nil {
								container.VolumeMounts = make([]*runv1.VolumeMount, 0)
							}
							container.VolumeMounts = append(container.VolumeMounts, &runv1.VolumeMount{
								Name:      secretName,
								MountPath: mountPath,
							})
						}

						// Update existing volume or add new one
						found = false
						if service.Spec.Template.Spec.Volumes != nil {
							for _, vol := range service.Spec.Template.Spec.Volumes {
								if vol.Name == secretName {
									vol.Secret.SecretName = secretName
									vol.Secret.Items[0].Key = version
									found = true
									break
								}
							}
						}
						if !found {
							if service.Spec.Template.Spec.Volumes == nil {
								service.Spec.Template.Spec.Volumes = make([]*runv1.Volume, 0)
							}
							service.Spec.Template.Spec.Volumes = append(service.Spec.Template.Spec.Volumes, &runv1.Volume{
								Name: secretName,
								Secret: &runv1.SecretVolumeSource{
									SecretName: secretName,
									Items: []*runv1.KeyToPath{{
										Key:  version,
										Path: filepath.Base(mountPath),
									}},
								},
							})
						}
					}
				} else {
					// Set as environment variable using ValueFrom
					secretName, version := parseSecretReference(v)

					// Only proceed if we have a valid secretName
					if secretName != "" {
						// Check if the environment variable already exists
						found := false
						if container.Env != nil {
							for _, env := range container.Env {
								if env.Name == k {
									if env.ValueFrom == nil {
										env.Value = ""
										env.ValueFrom = &runv1.EnvVarSource{
											SecretKeyRef: &runv1.SecretKeySelector{
												Key:  version,
												Name: secretName,
												LocalObjectReference: &runv1.LocalObjectReference{
													Name: secretName,
												},
											},
										}
									} else if env.ValueFrom.SecretKeyRef != nil {
										env.ValueFrom.SecretKeyRef.Key = version
										env.ValueFrom.SecretKeyRef.Name = secretName
										env.ValueFrom.SecretKeyRef.LocalObjectReference.Name = secretName
									}
									found = true
									break
								}
							}
						}

						if !found {
							if container.Env == nil {
								container.Env = make([]*runv1.EnvVar, 0)
							}
							container.Env = append(container.Env, &runv1.EnvVar{
								Name: k,
								ValueFrom: &runv1.EnvVarSource{
									SecretKeyRef: &runv1.SecretKeySelector{
										Key:  version,
										Name: secretName,
										LocalObjectReference: &runv1.LocalObjectReference{
											Name: secretName,
										},
									},
								},
							})
						}
					}
				}
			}
		}
	}

	// Set ingress traffic policy if specified
	if opts.Ingress != "" {
		// Make sure metadata annotations exist
		if service.Metadata.Annotations == nil {
			service.Metadata.Annotations = make(map[string]string)
		}

		// Set the ingress value
		switch opts.Ingress {
		case "internal":
			service.Metadata.Annotations["run.googleapis.com/ingress"] = "internal"
		case "internal-and-cloud-load-balancing":
			service.Metadata.Annotations["run.googleapis.com/ingress"] = "internal-and-cloud-load-balancing"
		default: // "all" is the default
			service.Metadata.Annotations["run.googleapis.com/ingress"] = "all"
		}
		log.Printf("Setting ingress to: %s", opts.Ingress)
	}

	// Set authentication policy
	if service.Metadata.Annotations == nil {
		service.Metadata.Annotations = make(map[string]string)
	}

	if opts.AllowUnauthenticated {
		service.Metadata.Annotations["run.googleapis.com/ingress-status"] = "all"
		log.Println("Allowing unauthenticated access")
	} else {
		service.Metadata.Annotations["run.googleapis.com/ingress-status"] = "internal-and-cloud-load-balancing"
		log.Println("Requiring authentication for access")
	}

	// Set the default URL setting
	if !opts.DefaultURL {
		service.Metadata.Annotations["run.googleapis.com/launch-stage"] = "BETA"
		service.Metadata.Annotations["run.googleapis.com/default-url"] = "disabled"
		log.Println("Disabling the default URL")
	} else {
		// Remove the annotation if it exists to enable default URL (which is the default behavior)
		delete(service.Metadata.Annotations, "run.googleapis.com/default-url")
		log.Println("Enabling the default URL")
	}
}

// buildServiceDefinition creates a new Cloud Run Service object based on the
// provided projectID and DeployOptions.
func buildServiceDefinition(projectID string, opts config.DeployOptions) runv1.Service {
	container := &runv1.Container{Image: opts.Image}

	// Set environment variables if provided and not empty
	if opts.EnvVars != nil && len(opts.EnvVars) > 0 {
		container.Env = make([]*runv1.EnvVar, 0, len(opts.EnvVars))
		for k, v := range opts.EnvVars {
			container.Env = append(container.Env, &runv1.EnvVar{
				Name:  k,
				Value: v,
			})
		}
	} else {
		// Initialize an empty env array to avoid null in JSON
		container.Env = make([]*runv1.EnvVar, 0)
	}

	// Set secrets if provided and not empty
	var volumes []*runv1.Volume
	if opts.Secrets != nil && len(opts.Secrets) > 0 {
		// Make sure Env is initialized
		if container.Env == nil {
			container.Env = make([]*runv1.EnvVar, 0)
		}
		container.VolumeMounts = make([]*runv1.VolumeMount, 0)
		volumes = make([]*runv1.Volume, 0)

		for k, v := range opts.Secrets {
			if k[0] == '/' {
				// Mount secret as volume
				mountPath := k
				secretName, version := parseSecretReference(v)

				// Only proceed if we have a valid secretName
				if secretName != "" {
					container.VolumeMounts = append(container.VolumeMounts, &runv1.VolumeMount{
						Name:      secretName,
						MountPath: mountPath,
					})

					volumes = append(volumes, &runv1.Volume{
						Name: secretName,
						Secret: &runv1.SecretVolumeSource{
							SecretName: secretName,
							Items: []*runv1.KeyToPath{{
								Key:  version,
								Path: filepath.Base(mountPath),
							}},
						},
					})
				}
			} else {
				// Set as environment variable using ValueFrom
				secretName, version := parseSecretReference(v)

				// Only proceed if we have a valid secretName
				if secretName != "" {
					log.Printf("Building service: Adding secret env var %s with secret %s and version %s", k, secretName, version)

					// Create a properly initialized SecretKeySelector with all required fields
					secretKeyRef := &runv1.SecretKeySelector{
						Key:  version,
						Name: secretName,
						LocalObjectReference: &runv1.LocalObjectReference{
							Name: secretName,
						},
					}

					// Use explicit field initialization to avoid null values in JSON
					container.Env = append(container.Env, &runv1.EnvVar{
						Name:  k,
						Value: "", // Ensure value is empty string, not null
						ValueFrom: &runv1.EnvVarSource{
							SecretKeyRef: secretKeyRef,
						},
					})

					// Log the resulting structure to verify it's set correctly
					log.Printf("Secret reference set for new service: Name=%s, Key=%s",
						secretName, version)
				}
			}
		}
	}

	// Create the service
	rService := runv1.Service{
		ApiVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: &runv1.ObjectMeta{
			Namespace:   projectID,
			Name:        opts.Service,
			Annotations: make(map[string]string),
		},
		Spec: &runv1.ServiceSpec{
			Template: &runv1.RevisionTemplate{
				Spec: &runv1.RevisionSpec{
					Containers: []*runv1.Container{container},
					// Only set volumes if we have any
					Volumes: volumes,
				},
			},
		},
	}

	// Set ingress traffic policy
	switch opts.Ingress {
	case "internal":
		rService.Metadata.Annotations["run.googleapis.com/ingress"] = "internal"
		log.Printf("Setting ingress to: internal")
	case "internal-and-cloud-load-balancing":
		rService.Metadata.Annotations["run.googleapis.com/ingress"] = "internal-and-cloud-load-balancing"
		log.Printf("Setting ingress to: internal-and-cloud-load-balancing")
	default: // "all" is the default
		rService.Metadata.Annotations["run.googleapis.com/ingress"] = "all"
		log.Printf("Setting ingress to: all")
	}

	// Set authentication policy
	if opts.AllowUnauthenticated {
		rService.Metadata.Annotations["run.googleapis.com/ingress-status"] = "all"
		log.Println("Allowing unauthenticated access")
	} else {
		rService.Metadata.Annotations["run.googleapis.com/ingress-status"] = "internal-and-cloud-load-balancing"
		log.Println("Requiring authentication for access")
	}

	// Set the default URL setting
	if !opts.DefaultURL {
		rService.Metadata.Annotations["run.googleapis.com/launch-stage"] = "BETA"
		rService.Metadata.Annotations["run.googleapis.com/default-url"] = "disabled"
		log.Println("Disabling the default URL")
	} else {
		log.Println("Enabling the default URL (default behavior)")
	}

	return rService
}

// CreateOrUpdateServiceV2 deploys a service to Cloud run using the v2 API.
// If the service doesn't exist, it creates a new one. If the service exists,
// it updates the existing service with the config.DeployOptions.
func CreateOrUpdateServiceV2(servicesClient *run.ProjectsLocationsServicesService, projectID, region string, opts config.DeployOptions) error {
	parent := fmt.Sprintf("projects/%s/locations/%s", projectID, region)
	name := fmt.Sprintf("%s/services/%s", parent, opts.Service)
	log.Printf("Deploying container to Cloud Run service [%s] in project [%s] region [%s]\n", opts.Service, projectID, region)

	var existingService *run.GoogleCloudRunV2Service
	var err error

	// Check if service exists
	getCall := servicesClient.Get(name)
	existingService, err = getCall.Do()

	if err != nil {
		// If the service doesn't exist, create a new one
		gErr, ok := err.(*googleapi.Error)
		if !ok || gErr.Code != http.StatusNotFound {
			return err
		}

		// Create new service
		log.Printf("Creating a new service %s\n", opts.Service)
		service := buildServiceDefinitionV2(projectID, opts)

		// IMPORTANT: Do not set the Name field directly in the service object
		// The API will extract the service_id from the parent path
		createCall := servicesClient.Create(parent, service)

		// Set the service ID explicitly as a parameter rather than in the body
		// This ensures the right format is used for the V2 API
		log.Printf("Using service ID: %s for Cloud Run V2 API", opts.Service)
		_, err = createCall.ServiceId(opts.Service).Do()
		if err != nil {
			log.Printf("Error creating service: %v", err)
			return err
		}

		// Set IAM policy for authentication if needed
		if opts.AllowUnauthenticated {
			if err := SetIAMPolicyV2(servicesClient, name, true); err != nil {
				log.Printf("Warning: Failed to set IAM policy to allow unauthenticated access: %v", err)
				// Continue despite the IAM policy error
			}
		}
	} else {
		// Update existing service
		log.Printf("Updating the existing service %s\n", opts.Service)
		updateServiceWithOptionsV2(existingService, opts)

		patchCall := servicesClient.Patch(name, existingService)
		_, err = patchCall.Do()

		if err != nil {
			return err
		}

		// Set IAM policy for authentication if needed
		if opts.AllowUnauthenticated {
			if err := SetIAMPolicyV2(servicesClient, name, true); err != nil {
				log.Printf("Warning: Failed to set IAM policy to allow unauthenticated access: %v", err)
				// Continue despite the IAM policy error
			}
		}
	}

	return nil
}

// SetIAMPolicyV2 sets the IAM policy for a Cloud Run service to allow or deny
// unauthenticated access using the v2 API.
func SetIAMPolicyV2(servicesClient *run.ProjectsLocationsServicesService, serviceName string, allowUnauthenticated bool) error {
	log.Printf("Setting IAM policy for service: %s", serviceName)

	// Create the SetIamPolicy request
	policy := &run.GoogleIamV1Policy{
		Bindings: []*run.GoogleIamV1Binding{
			{
				// Role for invoking Cloud Run services
				Role:    "roles/run.invoker",
				Members: []string{
					// Add members who can invoke the service
				},
			},
		},
		// Version specifies the format of the policy
		Version: 3,
	}

	// Add allUsers for unauthenticated access
	if allowUnauthenticated {
		policy.Bindings[0].Members = append(policy.Bindings[0].Members, "allUsers")
		log.Println("Allowing unauthenticated access via IAM policy")
	} else {
		// For authenticated access, typically you'd add specific service accounts or groups
		// Here we're just showing an example with authenticated compute service account
		policy.Bindings[0].Members = append(policy.Bindings[0].Members, "serviceAccount:PROJECT_NUMBER-compute@developer.gserviceaccount.com")
		log.Println("Requiring authentication via IAM policy")
	}

	request := &run.GoogleIamV1SetIamPolicyRequest{
		Policy: policy,
	}

	// Call the SetIamPolicy method
	setIamPolicyCall := servicesClient.SetIamPolicy(serviceName, request)
	_, err := setIamPolicyCall.Do()
	if err != nil {
		return fmt.Errorf("failed to set IAM policy: %v", err)
	}

	log.Println("Successfully set IAM policy")
	return nil
}

// WaitForServiceReadyV2 waits for a Cloud Run service to reach a ready state
// by polling its status using the v2 API.
func WaitForServiceReadyV2(ctx context.Context, servicesClient *run.ProjectsLocationsServicesService, projectID, region, service string) error {
	name := fmt.Sprintf("projects/%s/locations/%s/services/%s", projectID, region, service)

	log.Println("Waiting for service to be ready (timeout: 2 minutes)...")

	if err := utils.PollWithInterval(ctx, time.Minute*2, time.Second*5, func() (bool, error) {
		getCall := servicesClient.Get(name)
		runService, err := getCall.Do()

		if err != nil {
			return false, err
		}

		// Log all conditions for debugging
		log.Println("Current service conditions:")
		for _, cond := range runService.Conditions {
			log.Printf("- Type: %s, State: %s, Message: %s", cond.Type, cond.State, cond.Message)

			if cond.Type == "Ready" {
				// Check for multiple success states
				if cond.State == "CONDITION_SUCCEEDED" ||
					strings.Contains(strings.ToLower(cond.Message), "ready") {
					return true, nil
				}

				// Only fail for explicit failure
				if cond.State == "CONDITION_FAILED" {
					return false, fmt.Errorf("failed to deploy: %s", cond.Message)
				}
			}
		}

		// Also check other indicators of readiness
		if runService.LatestReadyRevision != "" && runService.ObservedGeneration > 0 {
			log.Println("Service has a latest ready revision and observed generation > 0")
			return true, nil
		}

		return false, nil
	}); err != nil {
		return err
	}

	log.Println("Service is ready!")
	return nil
}

// updateServiceWithOptionsV2 updates a Cloud Run Service v2 with the specified options
func updateServiceWithOptionsV2(service *run.GoogleCloudRunV2Service, opts config.DeployOptions) {
	// Update the container image
	if opts.Image != "" && len(service.Template.Containers) > 0 {
		service.Template.Containers[0].Image = opts.Image
	}

	// Handle environment variables
	if opts.ClearEnvVars {
		if len(service.Template.Containers) > 0 {
			service.Template.Containers[0].Env = []*run.GoogleCloudRunV2EnvVar{}
		}
	} else if opts.EnvVars != nil && len(opts.EnvVars) > 0 {
		// Replace all environment variables
		envVars := make([]*run.GoogleCloudRunV2EnvVar, 0, len(opts.EnvVars))
		for k, v := range opts.EnvVars {
			envVars = append(envVars, &run.GoogleCloudRunV2EnvVar{
				Name:  k,
				Value: v,
			})
		}
		if len(service.Template.Containers) > 0 {
			service.Template.Containers[0].Env = envVars
		}
	} else if opts.UpdateEnvVars != nil && len(opts.UpdateEnvVars) > 0 {
		// Update specific environment variables
		if len(service.Template.Containers) > 0 {
			container := service.Template.Containers[0]
			// Create a map for easier lookup
			envMap := make(map[string]*run.GoogleCloudRunV2EnvVar)
			for _, env := range container.Env {
				envMap[env.Name] = env
			}

			// Update or add new variables
			for k, v := range opts.UpdateEnvVars {
				if existing, ok := envMap[k]; ok {
					existing.Value = v
				} else {
					container.Env = append(container.Env, &run.GoogleCloudRunV2EnvVar{
						Name:  k,
						Value: v,
					})
				}
			}
		}
	}

	// Remove specific environment variables if requested
	if len(opts.RemoveEnvVars) > 0 && len(service.Template.Containers) > 0 {
		container := service.Template.Containers[0]
		toRemove := make(map[string]bool)
		for _, envVar := range opts.RemoveEnvVars {
			toRemove[envVar] = true
		}

		newEnvs := make([]*run.GoogleCloudRunV2EnvVar, 0)
		for _, env := range container.Env {
			if !toRemove[env.Name] {
				newEnvs = append(newEnvs, env)
			}
		}
		container.Env = newEnvs
	}

	// Handle secrets
	if opts.ClearSecrets {
		// Remove all volume mounts and secret environment variables
		if len(service.Template.Containers) > 0 {
			container := service.Template.Containers[0]
			newEnvs := make([]*run.GoogleCloudRunV2EnvVar, 0)
			for _, env := range container.Env {
				// Keep non-secret environment variables
				if env.ValueSource == nil || env.ValueSource.SecretKeyRef == nil {
					newEnvs = append(newEnvs, env)
				}
			}
			container.Env = newEnvs

			// Clear volume mounts related to secrets
			newVolumeMounts := make([]*run.GoogleCloudRunV2VolumeMount, 0)
			for _, vm := range container.VolumeMounts {
				// Check if this mount is for a secret volume
				isSecretMount := false
				for _, vol := range service.Template.Volumes {
					if vol.Name == vm.Name && vol.Secret != nil {
						isSecretMount = true
						break
					}
				}

				if !isSecretMount {
					newVolumeMounts = append(newVolumeMounts, vm)
				}
			}
			container.VolumeMounts = newVolumeMounts
		}

		// Remove secret volumes
		newVolumes := make([]*run.GoogleCloudRunV2Volume, 0)
		for _, vol := range service.Template.Volumes {
			if vol.Secret == nil {
				newVolumes = append(newVolumes, vol)
			}
		}
		service.Template.Volumes = newVolumes
	}

	// Handle secrets (both environment variables and volumes)
	if opts.Secrets != nil && len(opts.Secrets) > 0 {
		processSecretsV2(service, opts.Secrets)
	}

	// Set ingress setting using V2 API specific field
	// Configure ingress based on the option
	switch opts.Ingress {
	case "internal":
		service.Ingress = "INGRESS_TRAFFIC_INTERNAL_ONLY"
		log.Println("Setting ingress to internal")
	case "internal-and-cloud-load-balancing":
		service.Ingress = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
		log.Println("Setting ingress to internal-and-cloud-load-balancing")
	default: // "all"
		service.Ingress = "INGRESS_TRAFFIC_ALL"
		log.Println("Setting ingress to all")
	}

	// Set authentication settings - in V2 API, authentication is managed through IAM policies
	// The actual IAM policy must be set separately after the service is created
	if opts.AllowUnauthenticated {
		log.Println("Authentication will be disabled via IAM policy after service creation")
	} else {
		log.Println("Authentication will be enabled via IAM policy after service creation")
	}

	// The default URL setting in V2 API
	if !opts.DefaultURL {
		service.LaunchStage = "BETA"
		service.DefaultUriDisabled = true
		log.Println("Disabling the default URL")
	} else {
		service.LaunchStage = ""
		service.DefaultUriDisabled = false
		log.Println("Enabling the default URL")
	}

	// Configure VPC connectivity if specified
	if opts.VpcConnector != "" || opts.VpcEgress != "" || (opts.VpcNetwork != "" && opts.VpcSubnetwork != "") {
		// Initialize VPC access if not already set
		if service.Template.VpcAccess == nil {
			service.Template.VpcAccess = &run.GoogleCloudRunV2VpcAccess{}
		}

		// According to the API requirements, we can set either connector OR network_interfaces, but not both
		if opts.VpcConnector != "" {
			// If connector is specified, use the connector approach
			log.Printf("Setting VPC connector to %s\n", opts.VpcConnector)
			service.Template.VpcAccess.Connector = opts.VpcConnector

			// Add a note that network/subnetwork settings are ignored when connector is specified
			if opts.VpcNetwork != "" && opts.VpcSubnetwork != "" {
				log.Printf("Note: VPC network/subnetwork settings are ignored when a connector is specified\n")
			}
		} else if opts.VpcNetwork != "" && opts.VpcSubnetwork != "" {
			// If no connector but network/subnetwork are specified, set up network interfaces
			log.Printf("Setting VPC network interfaces for network: %s, subnetwork: %s\n", opts.VpcNetwork, opts.VpcSubnetwork)
			networkInterface := &run.GoogleCloudRunV2NetworkInterface{
				Network:    opts.VpcNetwork,
				Subnetwork: opts.VpcSubnetwork,
			}
			service.Template.VpcAccess.NetworkInterfaces = []*run.GoogleCloudRunV2NetworkInterface{networkInterface}
		}

		// Set VPC egress setting if specified
		if opts.VpcEgress != "" {
			log.Printf("Setting VPC egress to %s\n", opts.VpcEgress)
			if opts.VpcEgress == "all-traffic" {
				service.Template.VpcAccess.Egress = "ALL_TRAFFIC"
			} else {
				service.Template.VpcAccess.Egress = "PRIVATE_RANGES_ONLY"
			}
		}
	}
}

// processSecretsV2 processes secrets for environment variables and mounted volumes using the v2 API
func processSecretsV2(service *run.GoogleCloudRunV2Service, secrets map[string]string) {
	if len(service.Template.Containers) == 0 {
		// No containers to add secrets to
		return
	}

	container := service.Template.Containers[0]

	// Initialize env array if nil
	if container.Env == nil {
		container.Env = make([]*run.GoogleCloudRunV2EnvVar, 0)
	}

	// Process each secret
	for key, secretRef := range secrets {
		secretName, version := parseSecretReference(secretRef)

		// Make sure we have valid values
		if secretName == "" {
			log.Printf("Warning: Invalid secret reference format for key %s: %s. Expected format: SECRET:VERSION", key, secretRef)
			continue
		}

		// Check if it's a volume mount (path starts with /)
		if strings.HasPrefix(key, "/") {
			// Create volume if it doesn't exist
			volumeName := fmt.Sprintf("secret-volume-%s", secretName)

			// Initialize volumes array if nil
			if service.Template.Volumes == nil {
				service.Template.Volumes = make([]*run.GoogleCloudRunV2Volume, 0)
			}

			// Find or create the volume
			var volume *run.GoogleCloudRunV2Volume
			for _, v := range service.Template.Volumes {
				if v.Name == volumeName {
					volume = v
					break
				}
			}

			if volume == nil {
				volume = &run.GoogleCloudRunV2Volume{
					Name: volumeName,
					Secret: &run.GoogleCloudRunV2SecretVolumeSource{
						Secret: secretName,
						Items: []*run.GoogleCloudRunV2VersionToPath{
							{
								Version: version,
								Path:    filepath.Base(key),
							},
						},
					},
				}
				service.Template.Volumes = append(service.Template.Volumes, volume)
			}

			// Initialize volume mounts array if nil
			if container.VolumeMounts == nil {
				container.VolumeMounts = make([]*run.GoogleCloudRunV2VolumeMount, 0)
			}

			// Create volume mount
			mount := &run.GoogleCloudRunV2VolumeMount{
				Name:      volumeName,
				MountPath: filepath.Dir(key),
			}

			// Add mount if it doesn't exist
			mountExists := false
			for _, m := range container.VolumeMounts {
				if m.Name == volumeName && m.MountPath == filepath.Dir(key) {
					mountExists = true
					break
				}
			}

			if !mountExists {
				container.VolumeMounts = append(container.VolumeMounts, mount)
			}
		} else {
			// It's an environment variable
			// Find if the environment variable already exists
			var envVar *run.GoogleCloudRunV2EnvVar
			for _, env := range container.Env {
				if env.Name == key {
					envVar = env
					break
				}
			}

			if envVar == nil {
				envVar = &run.GoogleCloudRunV2EnvVar{
					Name: key,
				}
				container.Env = append(container.Env, envVar)
			}

			// Set the secret reference
			envVar.Value = ""
			envVar.ValueSource = &run.GoogleCloudRunV2EnvVarSource{
				SecretKeyRef: &run.GoogleCloudRunV2SecretKeySelector{
					Secret:  secretName,
					Version: version,
				},
			}
		}
	}
}

// buildServiceDefinitionV2 creates a new Cloud Run service definition using the v2 API
func buildServiceDefinitionV2(projectID string, opts config.DeployOptions) *run.GoogleCloudRunV2Service {
	// Create a new service without setting the Name field
	// The Name will be properly set by the API based on the parent and serviceId parameters
	service := &run.GoogleCloudRunV2Service{
		Template: &run.GoogleCloudRunV2RevisionTemplate{
			Containers: []*run.GoogleCloudRunV2Container{
				{
					Image: opts.Image,
				},
			},
		},
		// Initialize annotations map but don't use it for system settings
		Annotations: make(map[string]string),
		// Use V2 API specific fields for traffic configuration
		Traffic: []*run.GoogleCloudRunV2TrafficTarget{
			{
				Type:    "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST",
				Percent: 100,
			},
		},
	}

	// Add environment variables if specified
	if opts.EnvVars != nil && len(opts.EnvVars) > 0 {
		container := service.Template.Containers[0]
		container.Env = make([]*run.GoogleCloudRunV2EnvVar, 0, len(opts.EnvVars))

		for k, v := range opts.EnvVars {
			container.Env = append(container.Env, &run.GoogleCloudRunV2EnvVar{
				Name:  k,
				Value: v,
			})
		}
	}

	// Configure ingress based on the option
	switch opts.Ingress {
	case "internal":
		service.Ingress = "INGRESS_TRAFFIC_INTERNAL_ONLY"
		log.Println("Setting ingress to internal")
	case "internal-and-cloud-load-balancing":
		service.Ingress = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
		log.Println("Setting ingress to internal-and-cloud-load-balancing")
	default: // "all"
		service.Ingress = "INGRESS_TRAFFIC_ALL"
		log.Println("Setting ingress to all")
	}

	// Set authentication settings - in V2 API, authentication is managed through IAM policies
	if opts.AllowUnauthenticated {
		log.Println("Authentication will be disabled via IAM policy after service creation")
	} else {
		log.Println("Authentication will be enabled via IAM policy after service creation")
	}

	// The default URL setting in V2 API
	if !opts.DefaultURL {
		service.LaunchStage = "BETA"
		service.DefaultUriDisabled = true
		log.Println("Disabling the default URL")
	} else {
		service.LaunchStage = ""
		service.DefaultUriDisabled = false
		log.Println("Enabling the default URL")
	}

	// Configure VPC connectivity if specified
	if opts.VpcConnector != "" || opts.VpcEgress != "" || (opts.VpcNetwork != "" && opts.VpcSubnetwork != "") {
		service.Template.VpcAccess = &run.GoogleCloudRunV2VpcAccess{}

		// According to the API requirements, we can set either connector OR network_interfaces, but not both
		if opts.VpcConnector != "" {
			// If connector is specified, use the connector approach
			log.Printf("Setting VPC connector to %s\n", opts.VpcConnector)
			service.Template.VpcAccess.Connector = opts.VpcConnector

			// Add a note that network/subnetwork settings are ignored when connector is specified
			if opts.VpcNetwork != "" && opts.VpcSubnetwork != "" {
				log.Printf("Note: VPC network/subnetwork settings are ignored when a connector is specified\n")
			}
		} else if opts.VpcNetwork != "" && opts.VpcSubnetwork != "" {
			// If no connector but network/subnetwork are specified, set up network interfaces
			log.Printf("Setting VPC network interfaces for network: %s, subnetwork: %s\n", opts.VpcNetwork, opts.VpcSubnetwork)
			networkInterface := &run.GoogleCloudRunV2NetworkInterface{
				Network:    opts.VpcNetwork,
				Subnetwork: opts.VpcSubnetwork,
			}
			service.Template.VpcAccess.NetworkInterfaces = []*run.GoogleCloudRunV2NetworkInterface{networkInterface}
		}

		// Set VPC egress setting if specified
		if opts.VpcEgress != "" {
			log.Printf("Setting VPC egress to %s\n", opts.VpcEgress)
			if opts.VpcEgress == "all-traffic" {
				service.Template.VpcAccess.Egress = "ALL_TRAFFIC"
			} else {
				service.Template.VpcAccess.Egress = "PRIVATE_RANGES_ONLY"
			}
		}
	}

	// Process secrets if specified
	if opts.Secrets != nil && len(opts.Secrets) > 0 {
		processSecretsV2(service, opts.Secrets)
	}

	return service
}

// Helper function to set annotations on a service
func setAnnotation(service *run.GoogleCloudRunV2Service, key, value string) {
	if service.Annotations == nil {
		service.Annotations = make(map[string]string)
	}
	service.Annotations[key] = value
}
