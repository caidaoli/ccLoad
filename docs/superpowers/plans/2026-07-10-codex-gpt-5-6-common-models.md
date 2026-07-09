# Codex GPT-5.6 Common Models Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `gpt-5.6-sol`, `gpt-5.6-luna`, and `gpt-5.6-terra` to the Codex common-model suggestions.

**Architecture:** Keep the existing single front-end data source. Extend only `COMMON_MODELS.codex`; `addCommonModels()` continues to own case-insensitive deduplication, row insertion, dirty-state updates, and user feedback.

**Tech Stack:** Vanilla JavaScript, Node.js `node:test`, Make.

## Global Constraints

- Preserve every existing Codex common model and its current order.
- Append the three new models in this exact order: `gpt-5.6-sol`, `gpt-5.6-luna`, `gpt-5.6-terra`.
- Do not modify model fetching, protocol conversion, pricing, or other channel presets.
- Do not add a source-text or private-constant test; validate with the existing front-end suite.

---

### Task 1: Extend the Codex common-model list

**Files:**
- Modify: `web/assets/js/channels-modals.js:2085-2089`
- Test: Existing `web/assets/js/*.test.js` suite via `make verify-web`

**Interfaces:**
- Consumes: `addCommonModels()` reads `COMMON_MODELS[channelType]` and deduplicates model names case-insensitively.
- Produces: `COMMON_MODELS.codex` contains the three GPT-5.6 model identifiers after all existing entries.

- [ ] **Step 1: Establish a clean front-end baseline**

Run:

```bash
make verify-web
```

Expected: command exits with status 0 and all existing Node.js tests pass.

- [ ] **Step 2: Append the model identifiers**

Replace the Codex array with:

```js
  codex: [
    'gpt-5.4',
    'gpt-5.4-mini',
    'gpt-5.5',
    'gpt-5.6-sol',
    'gpt-5.6-luna',
    'gpt-5.6-terra'
  ],
```

- [ ] **Step 3: Run the front-end verification**

Run:

```bash
make verify-web
```

Expected: command exits with status 0 and all existing Node.js tests pass.

- [ ] **Step 4: Verify the patch scope**

Run:

```bash
git diff --check
git diff -- web/assets/js/channels-modals.js
```

Expected: no whitespace errors; the production diff only appends the three requested strings and adds the comma required after `gpt-5.5`.

- [ ] **Step 5: Commit the implementation**

```bash
git add web/assets/js/channels-modals.js
git commit -m "feat: add GPT-5.6 Codex common models"
```
