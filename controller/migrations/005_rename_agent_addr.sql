-- Migration 005: Rename agent_addr → lan_addr in connectors
--
-- "agent_addr" was an internal implementation name. "lan_addr" is more
-- descriptive: it's the connector's LAN address that shields use to connect.

ALTER TABLE connectors RENAME COLUMN agent_addr TO lan_addr;
