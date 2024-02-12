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

package client

import (
	"context"
	"fmt"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

type IGCSClient interface {
	Bucket(name string) *storage.BucketHandle
}

type GCSClient struct {
	*storage.Client
}

func NewGCSClient(ctx context.Context, userAgent string) (IGCSClient, error) {
	c, err := storage.NewClient(ctx, option.WithUserAgent(userAgent))
	if err != nil {
		return nil, fmt.Errorf("GCS Client init error: %w", err)
	}
	return &GCSClient{Client: c}, err
}
