<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { fullStamp } from '@/lib/format'
import { accountState, beginReenroll, checkExistingCredential, login, revokeDevice } from '@/account'
import type { Device } from '@/wire/generated'

/**
 * CANT-38 — conventional forms over CANT-28/29/30/117's REST endpoints:
 * log in (enroll), name the device, see the session list, revoke one. The
 * logic lives in `@/account`; this is the rendering and the two fields a
 * person types into.
 */

const token = ref('')
const deviceName = ref('')
const canSubmit = computed(() => token.value.trim() !== '' && deviceName.value.trim() !== '' && !accountState.busy)

async function submit() {
  if (!canSubmit.value) return
  const ok = await login(token.value.trim(), deviceName.value.trim())
  if (ok) {
    token.value = ''
    deviceName.value = ''
  }
}

/** Oldest first, as `GET /devices` already serves them (CANT-117) — nothing
 *  reorders here. */
const devices = computed<Device[]>(() => accountState.devices)

/** Revoking your own current device is allowed and not special-cased on the
 *  server (CANT-117) — but it ends THIS tab's session, which is worth
 *  confirming rather than discovering. Every other row revokes at once. */
function confirmedRevoke(d: Device) {
  if (d.id === accountState.deviceId && !window.confirm(`Revoke ${d.name}? This is the device you're using now — you'll be signed out immediately.`)) return
  void revokeDevice(d.id)
}

// Never runs during smoke.ts's SSR render (onMounted does not fire under
// renderToString), so the app's first paint is always the honest default —
// the login form — and the real check happens once this mounts for real.
onMounted(() => {
  void checkExistingCredential()
})
</script>

<template>
  <section class="account">
    <template v-if="accountState.mode === 'login'">
      <header class="head">
        <h1 class="title">SIGN IN</h1>
        <p class="lede">
          Enter the enrollment token you were given, and name this device — a revocation list only helps if
          the rows say which phone they are.
        </p>
      </header>

      <form class="form" @submit.prevent="submit">
        <label class="field">
          <span class="label">ENROLLMENT TOKEN</span>
          <input
            v-model="token"
            class="input"
            type="text"
            autocomplete="off"
            spellcheck="false"
            placeholder="paste the token you were sent"
          />
        </label>

        <label class="field">
          <span class="label">DEVICE NAME</span>
          <input
            v-model="deviceName"
            class="input"
            type="text"
            autocomplete="off"
            placeholder="e.g. Rosa's Pixel"
          />
        </label>

        <p v-if="accountState.error" class="error">{{ accountState.error }}</p>

        <button class="primary" type="submit" :disabled="!canSubmit">
          {{ accountState.busy ? 'ENROLLING…' : 'ENROLL DEVICE' }}
        </button>
      </form>
    </template>

    <template v-else>
      <header class="head">
        <h1 class="title">SESSIONS</h1>
        <p class="lede">Every device signed in to your account. Revoke one to sign it out immediately.</p>
      </header>

      <div v-if="accountState.terminal.kind === 'credential'" class="terminal-banner">
        <span class="text">This device's credential is no longer valid — nothing here will refresh until you re-enroll.</span>
        <button class="ghost" @click="beginReenroll">RE-ENROLL</button>
      </div>

      <p v-if="accountState.sessionError" class="error" role="alert">{{ accountState.sessionError }}</p>
      <p v-if="accountState.error" class="error">{{ accountState.error }}</p>

      <p v-if="accountState.busy && !devices.length" class="empty">Loading your devices…</p>
      <p v-else-if="!devices.length" class="empty">No devices on this account yet.</p>

      <ul v-else class="devices">
        <li v-for="d in devices" :key="d.id" class="device" :data-device-id="d.id">
          <div class="top">
            <span class="name">{{ d.name }}</span>
            <span v-if="d.id === accountState.deviceId" class="this-device">THIS DEVICE</span>
            <button v-if="!d.revokedAt" class="revoke" :disabled="accountState.busy" @click="confirmedRevoke(d)">REVOKE</button>
          </div>
          <div class="bottom">
            <span class="meta">ENROLLED {{ fullStamp(d.createdAt) }}</span>
            <span v-if="d.revokedAt" class="marker fault">REVOKED {{ fullStamp(d.revokedAt) }}</span>
          </div>
        </li>
      </ul>
    </template>
  </section>
</template>

<style scoped>
.account {
  display: block;
  max-width: 480px;
  min-width: 0;
  padding: var(--s8) var(--s6);
  overflow: auto;
  overscroll-behavior: none;
  background: var(--surface-base);
}

.head {
  margin-bottom: var(--s6);
}

.title {
  font: var(--type-title);
  color: var(--text-primary);
}

.lede {
  margin-top: var(--s2);
  font: var(--type-secondary);
  color: var(--text-secondary);
}

.form {
  display: grid;
  gap: var(--s4);
}

.field {
  display: grid;
  gap: var(--s1);
}

.field .label {
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--text-meta);
}

.input {
  height: 38px;
  padding: 0 12px;
  font: var(--type-body);
  color: var(--text-primary);
  background: var(--surface-rail);
  border: 1px solid var(--line-edge);
  border-radius: var(--radius-input);
  outline: none;
}
.input:focus {
  border-color: var(--accent-wire);
}

.error {
  font: var(--type-secondary);
  color: var(--signal-fault);
}

.primary {
  justify-self: start;
  padding: 8px 16px;
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--on-accent);
  background: var(--accent-wire);
  border-radius: var(--radius-input);
}
.primary:disabled {
  color: var(--text-disabled);
  background: var(--surface-raised);
  cursor: not-allowed;
}

.terminal-banner {
  display: flex;
  gap: var(--s3);
  align-items: center;
  padding: var(--s3) var(--s4);
  margin-bottom: var(--s4);
  background: var(--accent-wash);
  border: 1px solid var(--accent-rule);
}
.terminal-banner .text {
  font: var(--type-secondary);
  color: var(--text-primary);
}
.terminal-banner .ghost {
  flex: none;
  margin-left: auto;
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--accent-wire);
}

.empty {
  font: var(--type-secondary);
  color: var(--text-meta);
}

.devices {
  display: grid;
  gap: 1px;
  background: var(--line-faint);
}

.device {
  display: grid;
  gap: 4px;
  padding: var(--s3) var(--s4);
  background: var(--surface-base);
}

.device .top,
.device .bottom {
  display: flex;
  gap: var(--s3);
  align-items: center;
}

.device .name {
  min-width: 0;
  overflow: hidden;
  font-size: 13px;
  color: var(--text-primary);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.device .meta {
  font: var(--type-meta);
  color: var(--text-meta);
}

.marker {
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.1em;
}
.marker.fault {
  color: var(--signal-fault);
}

.this-device {
  flex: none;
  padding: 2px 6px;
  font: var(--type-label);
  font-size: 9px;
  letter-spacing: 0.1em;
  color: var(--text-meta);
  border: 1px solid var(--line-edge);
}

.revoke {
  flex: none;
  margin-left: auto;
  padding: 3px 8px;
  font: var(--type-label);
  letter-spacing: 0.1em;
  color: var(--text-secondary);
  border: 1px solid var(--line-edge);
}
.revoke:hover {
  color: var(--signal-fault);
  border-color: var(--signal-fault);
}
.revoke:disabled {
  color: var(--text-disabled);
  cursor: not-allowed;
}

@media (max-width: 900px) {
  .account {
    max-width: none;
    padding: var(--s6) var(--s4);
  }
}
</style>
