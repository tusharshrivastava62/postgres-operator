/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package v1alpha1

import (
	"fmt"
	"net"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// validateCompliance holds the compliance rules the CRD schema cannot express,
// because they relate one field to another or to the backup settings.
func validateCompliance(cluster *dbv1alpha1.PostgresCluster) field.ErrorList {
	compliance := cluster.Spec.Compliance
	if compliance == nil {
		return nil
	}
	var errs field.ErrorList
	destPath := field.NewPath("spec", "compliance", "auditLogs", "destination")
	dest := compliance.AuditLogs.Destination

	errs = append(errs, validatePeers(field.NewPath("spec", "compliance", "network", "allowFrom"), compliance.Network.AllowFrom)...)

	if dest == nil {
		return append(errs, field.Required(destPath,
			"must be set when spec.compliance is set: audit logs need somewhere to go"))
	}
	errs = append(errs, validateRegion(destPath, dest)...)

	// Audit logs in the backups' path would be caught by anything that manages
	// the backups there, such as a lifecycle rule for their expiry, and a path
	// inside another is caught the same way.
	backup := cluster.Spec.Backup
	if backup.Enabled && backup.Destination != nil && backup.Destination.SameStore(dest) {
		own := backup.Destination.PathFor(cluster.Namespace, cluster.Name)
		audit := compliance.AuditPathFor(cluster.Namespace, cluster.Name)
		if dbv1alpha1.PathsOverlap(own, audit) {
			errs = append(errs, field.Invalid(destPath.Child("path"), audit, fmt.Sprintf(
				"must not be the same as, or inside, the path of this cluster's backups (%s): set a path that is separate from it", own)))
		}
	}
	return errs
}

// validatePeers checks the extra peers the network policy lets in. The API
// server refuses a NetworkPolicy with an empty peer, or one that mixes an IP
// block with selectors, and the operator would then fail to create the policy
// on every reconcile; refusing it here says so at once, at the field.
func validatePeers(path *field.Path, peers []networkingv1.NetworkPolicyPeer) field.ErrorList {
	var errs field.ErrorList
	for i, peer := range peers {
		at := path.Index(i)
		selectors := peer.PodSelector != nil || peer.NamespaceSelector != nil
		switch {
		case peer.IPBlock == nil && !selectors:
			errs = append(errs, field.Required(at,
				"must set podSelector, namespaceSelector or ipBlock: an empty peer names nobody"))
		case peer.IPBlock != nil && selectors:
			errs = append(errs, field.Invalid(at.Child("ipBlock"), peer.IPBlock.CIDR,
				"cannot be combined with podSelector or namespaceSelector in the same peer"))
		case peer.IPBlock != nil:
			if _, network, err := net.ParseCIDR(peer.IPBlock.CIDR); err != nil {
				errs = append(errs, field.Invalid(at.Child("ipBlock", "cidr"), peer.IPBlock.CIDR, "must be a CIDR, such as 10.0.0.0/16"))
			} else {
				errs = append(errs, validateExceptions(at.Child("ipBlock", "except"), network, peer.IPBlock.Except)...)
			}
		}
	}
	return errs
}

// validateExceptions requires every excepted range to lie inside the block it
// is cut out of, as the API server does.
func validateExceptions(path *field.Path, block *net.IPNet, except []string) field.ErrorList {
	var errs field.ErrorList
	for i, cidr := range except {
		ip, network, err := net.ParseCIDR(cidr)
		switch {
		case err != nil:
			errs = append(errs, field.Invalid(path.Index(i), cidr, "must be a CIDR, such as 10.0.1.0/24"))
		case !block.Contains(ip) || !blockContainsNetwork(block, network):
			errs = append(errs, field.Invalid(path.Index(i), cidr, "must lie inside the block it is excepted from"))
		}
	}
	return errs
}

func blockContainsNetwork(block, inner *net.IPNet) bool {
	blockOnes, _ := block.Mask.Size()
	innerOnes, _ := inner.Mask.Size()
	return block.Contains(inner.IP) && innerOnes >= blockOnes
}

// complianceWarnings returns advice about compliance settings that are
// allowed but worth a second look. old is nil on create.
func complianceWarnings(old, cluster *dbv1alpha1.PostgresCluster) admission.Warnings {
	var warnings admission.Warnings

	if compliance := cluster.Spec.Compliance; compliance != nil && compliance.AuditLogs.LogParameters {
		warnings = append(warnings,
			"spec.compliance.auditLogs.logParameters is true: the values bound to statement parameters are written to the "+
				"audit log in clear text, so any sensitive value a client sends will be in it")
	}

	// pgaudit has to be loaded when PostgreSQL starts, and the list of what to
	// load is in the pod template, so turning the settings on or off replaces
	// the pods. Moving between standards only changes settings that reload.
	if old != nil {
		before, after := standardOf(old), standardOf(cluster)
		switch {
		case before == after:
		case before == "" || after == "":
			warnings = append(warnings, fmt.Sprintf(
				"the compliance standard changed from %s to %s: the database pods will be replaced one at a time, replicas "+
					"first and the primary after a switchover, so writes are refused for a few seconds", describe(before), describe(after)))
		default:
			warnings = append(warnings, fmt.Sprintf(
				"the compliance standard changed from %s to %s: the new audit settings are applied to the running pods "+
					"without restarting them", describe(before), describe(after)))
		}
	}

	// The pods load pgaudit and the extensions Spilo loads by default, by name.
	// An image without them cannot start PostgreSQL.
	if cluster.Spec.Compliance != nil && cluster.Spec.Image != "" {
		warnings = append(warnings,
			"spec.image is set with spec.compliance: the image must contain pgaudit and every extension the default image "+
				"preloads, or the database pods will not start")
	}
	return warnings
}

func standardOf(cluster *dbv1alpha1.PostgresCluster) string {
	if cluster.Spec.Compliance == nil {
		return ""
	}
	return cluster.Spec.Compliance.Standard
}

func describe(standard string) string {
	if standard == "" {
		return "none"
	}
	return fmt.Sprintf("%q", standard)
}
