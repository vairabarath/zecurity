// Package usage is the Sprint 25 usage ledger (F32; Decision Record D-57,
// D-58, D-59): exact relayed bytes per tenant from R1's cumulative relay
// counters, session / connector / shield minutes and monthly active users,
// counted once per collected minute, never estimated. Schema: migration
// 071_usage.sql. The collector is U1b.
package usage
