-- Migration 006: Add lan_ip to shields
--
-- Stores the shield's auto-detected RFC-1918 LAN IP, reported via heartbeat.
-- Used for observability in the admin dashboard.

ALTER TABLE shields ADD COLUMN IF NOT EXISTS lan_ip TEXT;
