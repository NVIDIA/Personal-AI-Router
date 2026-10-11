<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Configuration

## Variables and Theme

The design tokens are supplied as CSS variables, which come from `base.css`. Users may also use `base-external.css` for OSS projects(changes the default font) but the `base.css` file includes:

- a CSS reset(identical to Tailwind's `preflight.css`)
- the design tokens, light(default, or `.light`, `.nv-light`), and dark theme(via `.dark`, `.nv-dark`, or `@media(prefers-color-scheme: dark)`)
- fonts, internal icons(not the full set from NVIDIA Brand Assets - just the minimal ones used in the internal-only `<Icon>` component)

This file can come from:

- `@kui/foundations-css`: the original source of this file, internal only package so not useful for external projects
- `@kui/foundations-react`: cloned from foundations-css to avoid needing to add another dep to have styles
- CDN

## Tailwind

To use KUI with Tailwind, we offer a Tailwind plugin. Install Tailwind (latest preferred over v3). Next, add the plugin to your Tailwind config.

For Tailwind v4, you'll need to add the KUI Foundations plugin to your global CSS file. Note we do **not** use the standard Tailwind import because we want to disable CSS layers and remove redundant CSS.

An example ideal `src/global.css` configuration:

```css
@import '@kui/foundations-react/base.css'; /* same as "@kui/foundations-css/base.css", equivalent to "base-external.css" which is for OSS projects */
@import '@kui/foundations-tailwind-plugin';
/**
 * Make sure you set `<html id="style-root">`
 *
 * This ensures your generated Tailwind utilities have higher specificity
 * and override component styles.
 */
#style-root {
    @tailwind utilities;
}

:root {
    /* ensure we're using correct NVIDIA colors by default */
    @apply bg-surface-base text-primary;
}
```

You should then find the root `<html>` element in your app and add `id="style-root"` to it, otherwise your generated Tailwind utilities will not work.
