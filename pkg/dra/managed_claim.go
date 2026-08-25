/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package dra

import (
	"crypto/sha256"
	"fmt"
	"io"

	v1 "kubevirt.io/api/core/v1"
)

const (
	// maxManagedClaimNameLen is the DNS subdomain limit that a ResourceClaim
	// name must respect.
	maxManagedClaimNameLen = 253
	// managedClaimHashLen is the number of hex characters of the name digest
	// appended when the name has to be truncated.
	managedClaimHashLen = 5
	// managedClaimComponentLen bounds how many characters of the VMI name and
	// the claim name each keep when the joined name has to be truncated, so
	// both stay recognizable. The two separators and the hash are reserved
	// from the budget and the remainder is split evenly:
	// 253 - managedClaimHashLen - 2 == 246, /2 == 123 per component.
	managedClaimComponentLen = (maxManagedClaimNameLen - managedClaimHashLen - 2) / 2
)

// ManagedClaimName returns the deterministic name of the ResourceClaim
// generated for a managed claim entry: <vmiName>-<claimName>.
//
// When that exceeds the DNS subdomain limit, a fixed-length prefix of both
// the VMI name and the claim name is kept and a SHA-256 digest of the full
// untruncated name is appended. Keeping a prefix of both names leaves each
// recognizable even when one is long, and the digest keeps two VMIs sharing
// a long prefix from colliding onto the same ResourceClaim.
//
// This name is a contract between two independently-rolled binaries:
// virt-controller renders it into the launcher pod's resourceClaims entry,
// while the provisioner controller uses it to name the ResourceClaim it
// creates. Changing the algorithm breaks claim binding during a rolling
// upgrade, so it must be treated as frozen once released.
func ManagedClaimName(vmiName, claimName string) string {
	name := fmt.Sprintf("%s-%s", vmiName, claimName)
	if len(name) <= maxManagedClaimNameLen {
		return name
	}

	hash := sha256.New()
	_, _ = io.WriteString(hash, name)
	digest := fmt.Sprintf("%x", hash.Sum(nil))[:managedClaimHashLen]

	return fmt.Sprintf("%s-%s-%s",
		truncateComponent(vmiName),
		truncateComponent(claimName),
		digest)
}

// truncateComponent keeps at most managedClaimComponentLen characters of a
// name component used to build a truncated ManagedClaimName.
func truncateComponent(s string) string {
	if len(s) <= managedClaimComponentLen {
		return s
	}
	return s[:managedClaimComponentLen]
}

// IsManagedClaim reports whether a VMI resource claim entry delegates claim
// generation to a ManagedClaimProvisioner.
func IsManagedClaim(claim v1.VirtualMachineInstanceResourceClaim) bool {
	return claim.ManagedClaimProvisionerName != nil && *claim.ManagedClaimProvisionerName != ""
}
