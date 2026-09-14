<script setup lang="ts">
import { useId } from 'vue'

/**
 * The logomark: canvas section 07, form 1C "Span". Two masts and the
 * messenger wire's sag — the actual catenary curve the product is named for —
 * knocked out of a solid tile. Orthogonal rects plus one curve on a 48 grid,
 * minimum 2px stroke, so it survives a 16px favicon; public/favicon.svg is
 * this same geometry.
 */
withDefaults(defineProps<{ size?: number }>(), { size: 18 })

/** One mask per instance. Two tiles on one page sharing an id would both
 *  resolve to whichever was painted first. */
const maskId = `logomark-${useId()}`
</script>

<template>
  <svg
    class="logomark"
    viewBox="0 0 48 48"
    :width="size"
    :height="size"
    aria-hidden="true"
  >
    <!-- The mask's white and black are luminance, not palette: white keeps
         the tile, black cuts the silhouette out of it. The tile's own color
         is the token, in the style block. -->
    <mask :id="maskId" maskUnits="userSpaceOnUse" x="0" y="0" width="48" height="48">
      <rect width="48" height="48" fill="#fff" />
      <g fill="#000">
        <rect x="10" y="12" width="3.4" height="28" />
        <rect x="34.6" y="12" width="3.4" height="28" />
        <rect x="5" y="31" width="38" height="2.4" />
      </g>
      <path d="M12 15 Q24 27 36 15" fill="none" stroke="#000" stroke-width="3" />
    </mask>
    <rect class="tile" width="48" height="48" :mask="`url(#${maskId})`" />
  </svg>
</template>

<style scoped>
.logomark {
  display: block;
  flex: none;
}

/* The brand tile, not a state: it does not compete with the one live
 * conductor, and on light grounds the token darkens so the cutout stays
 * visible. */
.tile {
  fill: var(--accent-mark);
}
</style>
