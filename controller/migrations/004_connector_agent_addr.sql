-- Migration 004: Add agent_addr to connectors
--
-- agent_addr is the address shields use to reach this connector (:9091).
-- Separate from public_ip (outbound internet IP) because shields on the same
-- LAN need the internal IP, not the ISP public IP.
-- Operator sets AGENT_ADDR in connector.conf; connector reports it via heartbeat.

ALTER TABLE connectors ADD COLUMN IF NOT EXISTS agent_addr TEXT;
