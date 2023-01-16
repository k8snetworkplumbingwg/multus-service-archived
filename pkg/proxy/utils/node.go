/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package utils provides utility function for multus-proxy
package utils

import (
	"strings"
)

// CheckNodeNameIdentical checks both strings point a same node
// it just checks hostname without domain
func CheckNodeNameIdentical(s1, s2 string) bool {
	return strings.Split(s1, ".")[0] == strings.Split(s2, ".")[0]
}
