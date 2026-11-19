<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Design Tokens

Design tokens are theme-aware values for colors, borders, and surfaces. Available via Tailwind utilities or CSS variables, both forms resolve to the same CSS variable. These tokens come from the "base.css" file from the Foundations React package or CDN link

| Example Token  | Tailwind utility (only when Tailwind is configured) | CSS variable (for raw CSS or non-Tailwind contexts) |
| -------------- | --------------------------------------------------- | --------------------------------------------------- |
| Sunken surface | `bg-surface-sunken`                                 | `--background-color-surface-sunken`                 |
| Primary text   | `text-primary`                                      | `--color-text-primary`                              |
| Base border    | `border-base`                                       | `--border-color-base`                               |

Always use these variables for custom styling — via Tailwind classes or direct CSS variable references.

## Themed Properties

Variables that automatically change per color theme (light/dark) or density theme (standard/compact/spacious).

### NVIDIA Color Theme

Semantic color theme variables. Prefer using these CSS variables so the UI automatically adapts to the current theme.

```css
--background-color-accent-blue: var(--color-blue-050); /* dark: var(--color-blue-900) */
--background-color-accent-blue-hover: var(--color-blue-050); /* dark: var(--color-blue-900) */
--background-color-accent-blue-selected: var(--color-blue-200); /* dark: var(--color-blue-800) */
--background-color-accent-blue-strong: var(--color-blue-700); /* dark: var(--color-blue-900) */
--background-color-accent-blue-subtle: var(--color-blue-050); /* dark: var(--color-blue-900) */
--background-color-accent-blue-subtle-hover: var(
    --color-blue-200
); /* dark: var(--color-blue-800) */
--background-color-accent-blue-subtle-selected: var(
    --color-blue-600
); /* dark: var(--color-blue-500) */
--background-color-accent-gray: var(--color-gray-200); /* dark: var(--color-gray-600) */
--background-color-accent-gray-hover: var(--color-gray-300); /* dark: var(--color-gray-1000) */
--background-color-accent-gray-selected: var(--color-gray-700); /* dark: var(--color-gray-200) */
--background-color-accent-gray-strong: var(--color-gray-700); /* dark: same */
--background-color-accent-gray-subtle: var(--color-gray-100); /* dark: var(--color-gray-700) */
--background-color-accent-gray-subtle-hover: var(
    --color-gray-300
); /* dark: var(--color-gray-200) */
--background-color-accent-gray-subtle-selected: var(
    --color-gray-700
); /* dark: var(--color-gray-600) */
--background-color-accent-green: var(--color-green-025); /* dark: var(--color-green-900) */
--background-color-accent-green-hover: var(--color-green-025); /* dark: var(--color-green-900) */
--background-color-accent-green-selected: var(--color-green-050); /* dark: var(--color-green-800) */
--background-color-accent-green-strong: var(--color-green-800); /* dark: var(--color-green-900) */
--background-color-accent-green-subtle: var(--color-green-100); /* dark: var(--color-green-900) */
--background-color-accent-green-subtle-hover: var(
    --color-green-200
); /* dark: var(--color-green-800) */
--background-color-accent-green-subtle-selected: var(--color-green-600); /* dark: same */
--background-color-accent-purple: var(--color-purple-200); /* dark: var(--color-purple-900) */
--background-color-accent-purple-hover: var(--color-purple-050); /* dark: var(--color-purple-900) */
--background-color-accent-purple-selected: var(
    --color-purple-100
); /* dark: var(--color-purple-800) */
--background-color-accent-purple-strong: var(
    --color-purple-700
); /* dark: var(--color-purple-900) */
--background-color-accent-purple-subtle: var(
    --color-purple-050
); /* dark: var(--color-purple-900) */
--background-color-accent-purple-subtle-hover: var(
    --color-purple-100
); /* dark: var(--color-purple-800) */
--background-color-accent-purple-subtle-selected: var(
    --color-purple-600
); /* dark: var(--color-purple-700) */
--background-color-accent-red: var(--color-red-100); /* dark: var(--color-red-900) */
--background-color-accent-red-hover: var(--color-red-050); /* dark: var(--color-red-900) */
--background-color-accent-red-selected: var(--color-red-100); /* dark: var(--color-red-800) */
--background-color-accent-red-strong: var(--color-red-700); /* dark: var(--color-red-900) */
--background-color-accent-red-subtle: var(--color-red-100); /* dark: var(--color-red-900) */
--background-color-accent-red-subtle-hover: var(--color-red-200); /* dark: var(--color-red-800) */
--background-color-accent-red-subtle-selected: var(
    --color-red-600
); /* dark: var(--color-red-700) */
--background-color-accent-teal: var(--color-teal-100); /* dark: var(--color-teal-900) */
--background-color-accent-teal-hover: var(--color-teal-050); /* dark: var(--color-teal-900) */
--background-color-accent-teal-selected: var(--color-teal-100); /* dark: var(--color-teal-800) */
--background-color-accent-teal-strong: var(--color-teal-800); /* dark: var(--color-teal-900) */
--background-color-accent-teal-subtle: var(--color-teal-100); /* dark: var(--color-teal-900) */
--background-color-accent-teal-subtle-hover: var(
    --color-teal-200
); /* dark: var(--color-teal-800) */
--background-color-accent-teal-subtle-selected: var(
    --color-teal-600
); /* dark: var(--color-teal-500) */
--background-color-accent-yellow: var(--color-yellow-100); /* dark: var(--color-yellow-900) */
--background-color-accent-yellow-hover: var(--color-yellow-050); /* dark: var(--color-yellow-900) */
--background-color-accent-yellow-selected: var(
    --color-yellow-100
); /* dark: var(--color-yellow-800) */
--background-color-accent-yellow-strong: var(
    --color-yellow-700
); /* dark: var(--color-yellow-900) */
--background-color-accent-yellow-subtle: var(
    --color-yellow-100
); /* dark: var(--color-yellow-900) */
--background-color-accent-yellow-subtle-hover: var(
    --color-yellow-200
); /* dark: var(--color-yellow-800) */
--background-color-accent-yellow-subtle-selected: var(--color-yellow-600); /* dark: same */
--background-color-background-contrast: var(--color-gray-900); /* dark: var(--color-gray-000) */
--background-color-background-default: var(--color-gray-000); /* dark: var(--color-gray-900) */
--background-color-background-emphasis: var(--color-gray-050); /* dark: var(--color-gray-700) */
--background-color-background-highlight: var(--color-gray-100); /* dark: var(--color-gray-600) */
--background-color-background-subtle: var(--color-gray-025); /* dark: var(--color-gray-800) */
--background-color-component-skeleton: var(--color-gray-100); /* dark: var(--color-gray-800) */
--background-color-component-skeleton-subtle: var(
    --color-gray-200
); /* dark: var(--color-gray-900) */
--background-color-component-tooltip: var(--color-gray-800); /* dark: same */
--background-color-component-track: var(
    --color-translucent-black-100
); /* dark: var(--color-translucent-white-200) */
--background-color-component-track-inverse: var(
    --color-translucent-black-100
); /* dark: var(--color-translucent-black-400) */
--background-color-feedback-danger: var(--color-red-300); /* dark: var(--color-red-900) */
--background-color-feedback-danger-hover: var(--color-red-600); /* dark: same */
--background-color-feedback-danger-pressed: var(--color-red-700); /* dark: same */
--background-color-feedback-danger-strong: var(--color-red-500); /* dark: same */
--background-color-feedback-danger-subtle-hover: var(
    --color-red-100
); /* dark: var(--color-red-900) */
--background-color-feedback-danger-subtle-pressed: var(
    --color-red-200
); /* dark: var(--color-red-800) */
--background-color-feedback-info: var(--color-blue-050); /* dark: var(--color-blue-950) */
--background-color-feedback-success: var(--color-green-025); /* dark: var(--color-green-950) */
--background-color-feedback-warning: var(--color-yellow-100); /* dark: var(--color-yellow-950) */
--background-color-interaction-base: var(
    --color-gray-000
); /* dark: var(--color-translucent-black-600) */
--background-color-interaction-disabled: var(
    --color-translucent-black-050
); /* dark: var(--color-translucent-white-100) */
--background-color-interaction-disabled-checked: var(
    --color-translucent-black-400
); /* dark: var(--color-translucent-white-500) */
--background-color-interaction-hover: var(
    --color-translucent-black-050
); /* dark: var(--color-translucent-white-100) */
--background-color-interaction-inverse: var(--color-gray-900); /* dark: var(--color-gray-000) */
--background-color-interaction-inverse-hover: var(
    --color-gray-950
); /* dark: var(--color-gray-100) */
--background-color-interaction-inverse-pressed: var(
    --color-gray-975
); /* dark: var(--color-gray-200) */
--background-color-interaction-pressed: var(
    --color-translucent-black-100
); /* dark: var(--color-translucent-white-200) */
--background-color-interaction-primary-base: var(--color-green-300); /* dark: same */
--background-color-interaction-primary-hover: var(--color-green-400); /* dark: same */
--background-color-interaction-primary-selected: var(--color-green-500); /* dark: same */
--background-color-interaction-selected: var(--color-gray-000); /* dark: var(--color-gray-1000) */
--background-color-surface-base: var(--color-gray-000); /* dark: var(--color-gray-1000) */
--background-color-surface-blanket: var(--color-translucent-black-700); /* dark: same */
--background-color-surface-glass: var(
    --color-translucent-white-800
); /* dark: var(--color-translucent-black-600) */
--background-color-surface-navigation: var(--color-gray-000); /* dark: var(--color-gray-1000) */
--background-color-surface-overlay: var(--color-gray-000); /* dark: var(--color-gray-900) */
--background-color-surface-raised: var(--color-gray-000); /* dark: var(--color-gray-950) */
--background-color-surface-sunken: var(--color-gray-025); /* dark: var(--color-gray-975) */
--border-color-accent-black: var(--color-gray-1000); /* dark: same */
--border-color-accent-blue: var(--color-blue-600); /* dark: var(--color-blue-300) */
--border-color-accent-gray: var(--color-gray-700); /* dark: var(--color-gray-300) */
--border-color-accent-green: var(--color-green-700); /* dark: var(--color-green-300) */
--border-color-accent-purple: var(--color-purple-600); /* dark: var(--color-purple-300) */
--border-color-accent-red: var(--color-red-600); /* dark: var(--color-red-300) */
--border-color-accent-teal: var(--color-teal-600); /* dark: var(--color-teal-300) */
--border-color-accent-white: var(--color-gray-000); /* dark: same */
--border-color-accent-yellow: var(--color-yellow-600); /* dark: var(--color-yellow-300) */
--border-color-base: var(
    --color-translucent-black-200
); /* dark: var(--color-translucent-white-200) */
--border-color-brand: var(--color-brand); /* dark: same */
--border-color-component-tooltip: var(--color-gray-600); /* dark: same */
--border-color-disabled: var(
    --color-translucent-black-400
); /* dark: var(--color-translucent-white-300) */
--border-color-feedback-danger: var(--color-red-500); /* dark: same */
--border-color-feedback-danger-hover: var(--color-red-600); /* dark: same */
--border-color-feedback-danger-strong: var(--color-red-700); /* dark: same */
--border-color-feedback-danger-subtle: var(--color-red-500); /* dark: var(--color-red-300) */
--border-color-feedback-info: var(--color-blue-400); /* dark: same */
--border-color-feedback-success: var(--color-green-400); /* dark: same */
--border-color-feedback-warning: var(--color-yellow-400); /* dark: var(--color-yellow-200) */
--border-color-interaction-base: var(
    --color-translucent-black-300
); /* dark: var(--color-translucent-white-200) */
--border-color-interaction-disabled: var(
    --color-translucent-black-100
); /* dark: var(--color-translucent-white-100) */
--border-color-interaction-hover: var(
    --color-translucent-black-900
); /* dark: var(--color-translucent-white-400) */
--border-color-interaction-inverse: var(--color-gray-900); /* dark: var(--color-gray-000) */
--border-color-interaction-inverse-hover: var(--color-gray-950); /* dark: var(--color-gray-100) */
--border-color-interaction-inverse-pressed: var(--color-gray-975); /* dark: var(--color-gray-200) */
--border-color-interaction-pressed: var(
    --color-translucent-black-900
); /* dark: var(--color-translucent-white-400) */
--border-color-interaction-primary-base: var(--color-green-300); /* dark: same */
--border-color-interaction-primary-hover: var(--color-green-400); /* dark: same */
--border-color-interaction-primary-selected: var(--color-green-500); /* dark: same */
--border-color-interaction-selected: var(--text-color-brand); /* dark: same */
--border-color-interaction-strong: var(
    --color-translucent-black-700
); /* dark: var(--color-translucent-white-700) */
--nv-current-theme: light; /* dark: dark */
--text-color-accent-black: var(--color-gray-1000); /* dark: same */
--text-color-accent-blue: var(--color-blue-700); /* dark: var(--color-blue-300) */
--text-color-accent-blue-strong: var(--color-blue-700); /* dark: var(--color-blue-200) */
--text-color-accent-blue-subtle: var(--color-blue-200); /* dark: same */
--text-color-accent-gray: var(--color-gray-700); /* dark: var(--color-gray-050) */
--text-color-accent-green: var(--color-green-700); /* dark: var(--color-green-300) */
--text-color-accent-green-strong: var(--color-green-700); /* dark: var(--color-green-200) */
--text-color-accent-green-subtle: var(--color-green-200); /* dark: same */
--text-color-accent-purple: var(--color-purple-700); /* dark: var(--color-purple-200) */
--text-color-accent-purple-strong: var(--color-purple-700); /* dark: var(--color-purple-200) */
--text-color-accent-purple-subtle: var(--color-purple-200); /* dark: same */
--text-color-accent-red: var(--color-red-700); /* dark: var(--color-red-300) */
--text-color-accent-red-strong: var(--color-red-700); /* dark: var(--color-red-200) */
--text-color-accent-red-subtle: var(--color-red-200); /* dark: same */
--text-color-accent-teal: var(--color-teal-700); /* dark: var(--color-teal-300) */
--text-color-accent-teal-strong: var(--color-teal-700); /* dark: var(--color-teal-200) */
--text-color-accent-teal-subtle: var(--color-teal-200); /* dark: same */
--text-color-accent-white: var(--color-gray-000); /* dark: same */
--text-color-accent-yellow: var(--color-yellow-700); /* dark: var(--color-yellow-300) */
--text-color-accent-yellow-strong: var(--color-yellow-700); /* dark: var(--color-yellow-200) */
--text-color-accent-yellow-subtle: var(--color-yellow-200); /* dark: same */
--text-color-base: var(--color-gray-600); /* dark: var(--color-gray-200) */
--text-color-brand: var(--color-brand); /* dark: same */
--text-color-component-nvidia-logo: var(--color-gray-1000); /* dark: var(--color-gray-000) */
--text-color-disabled: var(--color-gray-400); /* dark: var(--color-translucent-white-300) */
--text-color-feedback-danger: var(--color-red-500); /* dark: var(--color-red-400) */
--text-color-feedback-danger-inverse: var(--color-gray-900); /* dark: var(--color-red-300) */
--text-color-feedback-danger-strong: var(--color-red-700); /* dark: var(--color-red-300) */
--text-color-feedback-danger-subtle: var(--color-red-600); /* dark: var(--color-red-400) */
--text-color-feedback-info: var(--color-blue-500); /* dark: same */
--text-color-feedback-info-inverse: var(--color-gray-900); /* dark: var(--color-blue-300) */
--text-color-feedback-success: var(--color-green-500); /* dark: var(--color-green-400) */
--text-color-feedback-success-inverse: var(--color-gray-900); /* dark: var(--color-green-300) */
--text-color-feedback-warning: var(--color-yellow-500); /* dark: var(--color-yellow-300) */
--text-color-feedback-warning-inverse: var(--color-gray-900); /* dark: var(--color-yellow-200) */
--text-color-interaction-disabled-checked: var(
    --color-translucent-black-300
); /* dark: var(--color-translucent-black-600) */
--text-color-interaction-selected: var(
    --color-gray-000
); /* dark: var(--color-translucent-white-200) */
--text-color-inverse: var(--color-gray-000); /* dark: var(--color-gray-1000) */
--text-color-inverse-brand: var(--color-gray-950); /* dark: var(--text-color-brand) */
--text-color-placeholder: var(--color-gray-500); /* dark: var(--color-gray-400) */
--text-color-primary: var(--color-gray-1000); /* dark: var(--color-gray-000) */
--text-color-secondary: var(--color-gray-600); /* dark: var(--color-gray-300) */
--text-color-strong: var(--color-gray-900); /* dark: var(--color-gray-000) */
--text-color-subtle: var(--color-gray-400); /* dark: same */
```

Tailwind usage: `bg-surface-raised`, `text-accent-blue`.

### Density Theme

KUI offers a standard semantic sizing theme (`standard`) along with `compact` and `spacious` variants. Prefer these over fixed sizes for a consistent spacing system.

```css
--radius-density-xl: var(--radius-xl); /* compact(var(--radius-lg)), spacious(var(--radius-xl)) */
--spacing-density-xxs: 2px; /* compact(1px), spacious(4px) */
--spacing-density-xs: 4px; /* compact(2px), spacious(6px) */
--spacing-density-sm: 6px; /* compact(4px), spacious(8px) */
--spacing-density-md: 8px; /* compact(6px), spacious(12px) */
--spacing-density-lg: 12px; /* compact(8px), spacious(16px) */
--spacing-density-xl: 16px; /* compact(12px), spacious(24px) */
--spacing-density-2xl: 24px; /* compact(16px), spacious(32px) */
--spacing-density-3xl: 32px; /* compact(24px), spacious(48px) */
--spacing-density-4xl: 48px; /* compact(32px), spacious(64px) */
--spacing-density-5xl: 64px; /* compact(48px), spacious(80px) */
```

## Semantic Text Utilities

Tailwind text utilities for typography. Use directly or via the `Text` component's `kind` prop.

Example: `<div className="text-body-regular-md" />` or `<Text kind="body/regular/md" />`.

```tailwindcss
.text-body-bold-2xl { @apply font-bold text-24 leading-lh-150; }
.text-body-bold-3xl { @apply font-bold text-32 leading-lh-150; }
.text-body-bold-lg { @apply font-bold text-16 leading-lh-150; }
.text-body-bold-md { @apply font-bold text-14 leading-lh-150; }
.text-body-bold-sm { @apply font-bold text-12 leading-lh-150; }
.text-body-bold-xl { @apply font-bold text-18 leading-lh-150; }
.text-body-bold-xs { @apply font-bold text-10 leading-lh-150; }
.text-body-italic-2xl { @apply italic text-24 leading-lh-150; }
.text-body-italic-3xl { @apply italic text-32 leading-lh-150; }
.text-body-italic-lg { @apply italic text-16 leading-lh-150; }
.text-body-italic-md { @apply italic text-14 leading-lh-150; }
.text-body-italic-sm { @apply italic text-12 leading-lh-150; }
.text-body-italic-xl { @apply italic text-18 leading-lh-150; }
.text-body-italic-xs { @apply italic text-10 leading-lh-150; }
.text-body-regular-2xl { @apply text-24 leading-lh-150; }
.text-body-regular-3xl { @apply text-32 leading-lh-150; }
.text-body-regular-lg { @apply text-16 leading-lh-150; }
.text-body-regular-md { @apply text-14 leading-lh-150; }
.text-body-regular-sm { @apply text-12 leading-lh-150; }
.text-body-regular-xl { @apply text-18 leading-lh-150; }
.text-body-regular-xs { @apply text-10 leading-lh-150; }
.text-body-semibold-2xl { @apply font-semibold text-24 leading-lh-150; }
.text-body-semibold-3xl { @apply font-semibold text-32 leading-lh-150; }
.text-body-semibold-lg { @apply font-semibold text-16 leading-lh-150; }
.text-body-semibold-md { @apply font-semibold text-14 leading-lh-150; }
.text-body-semibold-sm { @apply font-semibold text-12 leading-lh-150; }
.text-body-semibold-xl { @apply font-semibold text-18 leading-lh-150; }
.text-body-semibold-xs { @apply font-semibold text-10 leading-lh-150; }
.text-display-2xl { @apply font-bold text-64 leading-lh-125; }
.text-display-lg { @apply font-bold text-50 calc(62/50); }
.text-display-md { @apply font-bold text-44 leading-lh-125; }
.text-display-sm { @apply font-bold text-40 leading-lh-125; }
.text-display-xl { @apply font-bold text-56 leading-lh-125; }
.text-display-xs { @apply font-bold text-36 leading-lh-125; }
.text-label-bold-2xl { @apply font-bold text-24 leading-lh-125; }
.text-label-bold-3xl { @apply font-bold text-32 leading-lh-125; }
.text-label-bold-lg { @apply font-bold text-16 leading-lh-125; }
.text-label-bold-md { @apply font-bold text-14 calc(17/14); }
.text-label-bold-sm { @apply font-bold text-12 leading-lh-125; }
.text-label-bold-xl { @apply font-bold text-18 calc(22/18); }
.text-label-bold-xs { @apply font-bold text-10 calc(12/10); }
.text-label-light-2xl { @apply font-light text-24 leading-lh-125; }
.text-label-light-3xl { @apply font-light text-32 leading-lh-125; }
.text-label-light-lg { @apply font-light text-16 leading-lh-125; }
.text-label-light-md { @apply font-light text-14 calc(17/14); }
.text-label-light-sm { @apply font-light text-12 leading-lh-125; }
.text-label-light-xl { @apply font-light text-18 calc(22/18); }
.text-label-light-xs { @apply font-light text-10 calc(12/10); }
.text-label-regular-2xl { @apply text-24 leading-lh-125; }
.text-label-regular-3xl { @apply text-32 leading-lh-125; }
.text-label-regular-lg { @apply text-16 leading-lh-125; }
.text-label-regular-md { @apply text-14 calc(17/14); }
.text-label-regular-sm { @apply text-12 leading-lh-125; }
.text-label-regular-xl { @apply text-18 calc(22/18); }
.text-label-regular-xs { @apply text-10 calc(12/10); }
.text-label-semibold-2xl { @apply font-semibold text-24 leading-lh-125; }
.text-label-semibold-3xl { @apply font-semibold text-32 leading-lh-125; }
.text-label-semibold-lg { @apply font-semibold text-16 leading-lh-125; }
.text-label-semibold-md { @apply font-semibold text-14 calc(17/14); }
.text-label-semibold-sm { @apply font-semibold text-12 leading-lh-125; }
.text-label-semibold-xl { @apply font-semibold text-18 calc(22/18); }
.text-label-semibold-xs { @apply font-semibold text-10 calc(12/10); }
.text-mono-2xl { @apply font-mono text-24 leading-lh-150; }
.text-mono-lg { @apply font-mono text-16 leading-lh-150; }
.text-mono-md { @apply font-mono text-14 leading-lh-150; }
.text-mono-sm { @apply font-mono text-12 leading-lh-150; }
.text-mono-xl { @apply font-mono text-20 leading-lh-150; }
.text-title-2xl { @apply font-bold text-36 leading-lh-125; }
.text-title-lg { @apply font-bold text-28 leading-lh-125; }
.text-title-md { @apply font-bold text-24 leading-lh-125; }
.text-title-sm { @apply font-bold text-20 leading-lh-125; }
.text-title-xl { @apply font-bold text-32 leading-lh-125; }
.text-title-xs { @apply font-bold text-18 calc(22/18); }
```

## Primitive Variables

Core static CSS variables (not theme-dependent). Foundation for utility classes and themed properties.

```css
--spacing: 4px;
--color-black: #000000;
--color-white: #ffffff;
--color-brand: #76b900;
--color-red-050: #ffe9e9;
--color-red-100: #ffd7d7;
--color-red-200: #ffbbbb;
--color-red-300: #ff8181;
--color-red-400: #fe3f3f;
--color-red-500: #e52020;
--color-red-600: #c21e1e;
--color-red-700: #961515;
--color-red-800: #650b0b;
--color-red-900: #4b0404;
--color-red-950: #2d0100;
--color-yellow-050: #feeeb2;
--color-yellow-100: #fcde7b;
--color-yellow-200: #f9c500;
--color-yellow-300: #ef9100;
--color-yellow-400: #df6500;
--color-yellow-500: #d73d00;
--color-yellow-600: #b93100;
--color-yellow-700: #8d2600;
--color-yellow-800: #601600;
--color-yellow-900: #441000;
--color-yellow-950: #2d0b00;
--color-green-025: #dafb7d;
--color-green-050: #cfff40;
--color-green-100: #bff230;
--color-green-200: #a5de15;
--color-green-300: #76b900;
--color-green-400: #549a00;
--color-green-500: #3f8500;
--color-green-600: #327100;
--color-green-700: #265600;
--color-green-800: #193800;
--color-green-900: #142700;
--color-green-950: #0d1a00;
--color-teal-050: #adfcf8;
--color-teal-100: #9aefe5;
--color-teal-200: #3ae3c9;
--color-teal-300: #1dbba4;
--color-teal-400: #139a86;
--color-teal-500: #0d8473;
--color-teal-600: #097061;
--color-teal-700: #04554b;
--color-teal-800: #033831;
--color-teal-900: #022723;
--color-teal-950: #011a19;
--color-blue-050: #cbf5ff;
--color-blue-100: #aaccee;
--color-blue-200: #7cd7fe;
--color-blue-300: #10b1fb;
--color-blue-400: #008af9;
--color-blue-500: #0074df;
--color-blue-600: #0060c7;
--color-blue-700: #0046a4;
--color-blue-800: #002781;
--color-blue-900: #002050;
--color-blue-950: #00112c;
--color-purple-050: #fae9ff;
--color-purple-100: #f9d4ff;
--color-purple-200: #f0b9fd;
--color-purple-300: #cd8ef0;
--color-purple-400: #c359ef;
--color-purple-500: #a846db;
--color-purple-600: #952fc6;
--color-purple-700: #741d9d;
--color-purple-800: #4d1368;
--color-purple-900: #331344;
--color-purple-950: #1f0f27;
--color-fuchsia-050: #ffe8f9;
--color-fuchsia-100: #ffd3f2;
--color-fuchsia-200: #feb5ee;
--color-fuchsia-300: #fc79ca;
--color-fuchsia-400: #e050b7;
--color-fuchsia-500: #d2308e;
--color-fuchsia-600: #b62475;
--color-fuchsia-700: #8c1c55;
--color-fuchsia-800: #5d1337;
--color-fuchsia-900: #420d25;
--color-fuchsia-950: #2d0919;
--color-gray-000: #ffffff;
--color-gray-025: #f7f7f7;
--color-gray-050: #eeeeee;
--color-gray-100: #e0e0e0;
--color-gray-1000: #000000;
--color-gray-200: #cccccc;
--color-gray-300: #a7a7a7;
--color-gray-400: #898989;
--color-gray-500: #757575;
--color-gray-600: #636363;
--color-gray-700: #4b4b4b;
--color-gray-800: #313131;
--color-gray-900: #222222;
--color-gray-950: #161616;
--color-gray-975: #0c0c0c;
--color-translucent-black-000: #00000000;
--color-translucent-black-050: #0000000d;
--color-translucent-black-100: #0000001a;
--color-translucent-black-120: #0000001f;
--color-translucent-black-150: #00000026;
--color-translucent-black-200: #00000033;
--color-translucent-black-300: #0000004d;
--color-translucent-black-400: #00000066;
--color-translucent-black-500: #00000080;
--color-translucent-black-600: #00000099;
--color-translucent-black-700: #000000b2;
--color-translucent-black-800: #000000cc;
--color-translucent-black-900: #000000e5;
--color-translucent-white-000: #ffffff00;
--color-translucent-white-050: #ffffff0d;
--color-translucent-white-100: #ffffff1a;
--color-translucent-white-120: #ffffff1f;
--color-translucent-white-200: #ffffff33;
--color-translucent-white-250: #ffffff40;
--color-translucent-white-300: #ffffff4d;
--color-translucent-white-400: #ffffff66;
--color-translucent-white-500: #ffffff80;
--color-translucent-white-600: #ffffff99;
--color-translucent-white-700: #ffffffb2;
--color-translucent-white-800: #ffffffcc;
--color-translucent-white-900: #ffffffe5;
--font-sans:
    NVIDIA Sans, NVIDIA Sans Fallback, ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji',
    'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji';
--font-mono:
    JetBrains Mono, JetBrains Mono Fallback, ui-monospace, monospace, 'Apple Color Emoji',
    'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji';
--text-10: 0.625rem;
--text-12: 0.75rem;
--text-14: 0.875rem;
--text-16: 1rem;
--text-18: 1.125rem;
--text-20: 1.25rem;
--text-22: 1.375rem;
--text-24: 1.5rem;
--text-28: 1.75rem;
--text-32: 2rem;
--text-36: 2.25rem;
--text-40: 2.5rem;
--text-44: 2.75rem;
--text-48: 3rem;
--text-50: 3.125rem;
--text-56: 3.5rem;
--text-60: 3.75rem;
--text-64: 4rem;
--text-72: 4.5rem;
--text-80: 5rem;
--font-weight-light: 300;
--font-weight-regular: 400;
--font-weight-semibold: 500;
--font-weight-bold: 700;
--leading-lh-100: 1;
--leading-lh-125: 1.25;
--leading-lh-150: 1.5;
--leading-lh-175: 1.75;
--breakpoint-xs: 20rem;
--breakpoint-sm: 36rem;
--breakpoint-md: 48rem;
--breakpoint-lg: 62rem;
--breakpoint-xl: 75rem;
--breakpoint-2xl: 100rem;
--container-3xs: 16rem;
--container-2xs: 18rem;
--container-xs: 20rem;
--container-sm: 24rem;
--container-md: 28rem;
--container-lg: 32rem;
--container-xl: 36rem;
--container-2xl: 42rem;
--container-3xl: 48rem;
--container-4xl: 56rem;
--container-5xl: 64rem;
--container-6xl: 72rem;
--container-7xl: 80rem;
--border-width-none: 0px;
--border-width-1: 1px;
--border-width-2: 2px;
--border-width-3: 3px;
--border-width-4: 4px;
--radius-none: 0px;
--radius-sm: 2px;
--radius-md: 4px;
--radius-lg: 8px;
--radius-xl: 16px;
--radius-2xl: 24px;
--radius-3xl: 32px;
--radius-round: 9999px;
--shadow-lg: 0px 8px 12px 0px var(--color-translucent-black-150);
--shadow-md: 0px 4px 6px 0px var(--color-translucent-black-120);
--shadow-sm: 0px 2px 4px 0px var(--color-translucent-black-120);
--animate-pulse: pulse 2s cubic-bezier(0.4, 0, 0.6, 1) infinite;
--animate-spin: spin 1s linear infinite;
--transition-duration-150: 150ms;
--transition-duration-200: 200ms;
--transition-duration-250: 250ms;
--transition-duration-300: 300ms;
--ease-out: cubic-bezier(0.4, 0, 0.2, 1);
```

Available as Tailwind classes: `--color-red-500` → `bg-red-500`, `--text-16` → `text-16`.
