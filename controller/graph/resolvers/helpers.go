package resolvers

// helpers.go — shared scan + format helpers used by connector/remote-network resolvers.
//
// Lives in a separate file from schema.resolvers.go so gqlgen does NOT move this
// code around or wrap it in deletion-warning comments when regenerating resolvers.
// gqlgen only touches files it generates; hand-written files here are untouched.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/resource"
)

const rfc3339 = "2006-01-02T15:04:05Z07:00"

func fmtTime(t time.Time) string { return t.Format(rfc3339) }

func fmtTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(rfc3339)
	return &s
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRemoteNetwork(s scanner) (*graph.RemoteNetwork, error) {
	var (
		rn        graph.RemoteNetwork
		location  string
		status    string
		createdAt time.Time
	)
	if err := s.Scan(&rn.ID, &rn.Name, &location, &status, &createdAt); err != nil {
		return nil, err
	}
	rn.Location = graph.NetworkLocation(strings.ToUpper(location))
	if !rn.Location.IsValid() {
		return nil, fmt.Errorf("invalid network location: %q", location)
	}
	rn.Status = graph.RemoteNetworkStatus(strings.ToUpper(status))
	if !rn.Status.IsValid() {
		return nil, fmt.Errorf("invalid remote network status: %q", status)
	}
	rn.CreatedAt = fmtTime(createdAt)
	rn.Connectors = []*graph.Connector{}
	rn.Shields = []*graph.Shield{}
	rn.NetworkHealth = graph.NetworkHealthOffline
	return &rn, nil
}

func scanConnector(s scanner) (*graph.Connector, error) {
	var (
		c            graph.Connector
		status       string
		lastSeenAt   *time.Time
		certNotAfter *time.Time
		createdAt    time.Time
	)
	if err := s.Scan(
		&c.ID, &c.Name, &status, &c.RemoteNetworkID,
		&lastSeenAt, &c.Version, &c.Hostname, &c.PublicIP, &c.LanAddr,
		&certNotAfter, &createdAt,
	); err != nil {
		return nil, err
	}
	c.Status = graph.ConnectorStatus(strings.ToUpper(status))
	if !c.Status.IsValid() {
		return nil, fmt.Errorf("invalid connector status: %q", status)
	}
	c.CreatedAt = fmtTime(createdAt)
	c.LastSeenAt = fmtTimePtr(lastSeenAt)
	c.CertNotAfter = fmtTimePtr(certNotAfter)
	return &c, nil
}

func computeNetworkHealth(connectors []*graph.Connector) graph.NetworkHealth {
	if len(connectors) == 0 {
		return graph.NetworkHealthOffline
	}
	for _, c := range connectors {
		if c.Status == graph.ConnectorStatusActive {
			return graph.NetworkHealthOnline
		}
	}
	return graph.NetworkHealthDegraded
}

func scanShield(s scanner) (*graph.Shield, error) {
	var (
		sh           graph.Shield
		status       string
		lastSeenAt   *time.Time
		certNotAfter *time.Time
		createdAt    time.Time
	)
	if err := s.Scan(
		&sh.ID, &sh.Name, &status, &sh.RemoteNetworkID, &sh.ConnectorID,
		&lastSeenAt, &sh.Version, &sh.Hostname, &sh.PublicIP,
		&sh.InterfaceAddr, &certNotAfter, &createdAt,
	); err != nil {
		return nil, err
	}
	sh.Status = graph.ShieldStatus(strings.ToUpper(status))
	if !sh.Status.IsValid() {
		return nil, fmt.Errorf("invalid shield status: %q", status)
	}
	sh.CreatedAt = fmtTime(createdAt)
	sh.LastSeenAt = fmtTimePtr(lastSeenAt)
	sh.CertNotAfter = fmtTimePtr(certNotAfter)
	return &sh, nil
}

func (r *queryResolver) loadShields(ctx context.Context, tenantID, remoteNetworkID string) ([]*graph.Shield, error) {
	rows, err := r.TenantDB.Query(ctx,
		`SELECT id, name, status, remote_network_id, connector_id,
		        last_heartbeat_at, version, hostname, public_ip,
		        interface_addr, cert_not_after, created_at
		   FROM shields
		  WHERE remote_network_id = $1
		    AND tenant_id = $2
		  ORDER BY created_at DESC`,
		remoteNetworkID, tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*graph.Shield
	for rows.Next() {
		sh, err := scanShield(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, sh)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		result = []*graph.Shield{}
	}
	return result, nil
}

func toResourceGQL(row *resource.Row) *graph.Resource {
	res := &graph.Resource{
		ID:             row.ID,
		Name:           row.Name,
		Description:    row.Description,
		Host:           row.Host,
		Protocol:       row.Protocol,
		PortFrom:       row.PortFrom,
		PortTo:         row.PortTo,
		Status:         row.Status,
		ErrorMessage:   row.ErrorMessage,
		AppliedAt:      fmtTimePtr(row.AppliedAt),
		LastVerifiedAt: fmtTimePtr(row.LastVerifiedAt),
		CreatedAt:      fmtTime(row.CreatedAt),
		RemoteNetwork: &graph.RemoteNetwork{
			ID:   row.RemoteNetworkID,
			Name: row.NetworkName,
		},
	}
	if row.ShieldName != nil {
		res.Shield = &graph.Shield{
			ID:     row.ShieldID,
			Name:   *row.ShieldName,
			Status: graph.ShieldStatus(strings.ToUpper(*row.ShieldStatus)),
		}
	}
	return res
}

func (r *queryResolver) loadConnectors(ctx context.Context, tenantID, remoteNetworkID string) ([]*graph.Connector, error) {
	rows, err := r.TenantDB.Query(ctx,
		`SELECT id, name, status, remote_network_id,
		        last_heartbeat_at, version, hostname, public_ip, lan_addr,
		        cert_not_after, created_at
		   FROM connectors
		  WHERE remote_network_id = $1
		    AND tenant_id = $2
		  ORDER BY created_at DESC`,
		remoteNetworkID, tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*graph.Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		result = []*graph.Connector{}
	}
	return result, nil
}
