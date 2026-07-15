# Channel TTFB Priority Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional channel-level first-byte (TTFB) penalty into effective priority scoring using relative median slowness among same-request candidates.

**Architecture:** Extend `ChannelHealthStats` + success-rate query with TTFB aggregates; load new settings into `HealthScoreConfig`; compute median L_med over current candidates in `sortChannelsByHealth`; subtract TTFB penalty in `calculateEffectivePriority`. Defaults off.

**Tech Stack:** Go, existing health cache / selector balancer, SQLite/MySQL logs aggregate SQL, system_settings seed.

**Spec:** `docs/superpowers/specs/2026-07-15-channel-ttfb-priority-design.md`

## Tasks overview

1. Model + settings defaults
2. SQL aggregate TTFB into GetChannelSuccessRates
3. Formula + median in selector_balancer
4. loadHealthScoreConfig + health cache default zero values
5. Unit/integration tests
6. Verify

