<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# ThemeProvider

A comprehensive theme provider that manages both density (spacing) and theme (light/dark) themes
for all child components. This component:

- Sets density variants that control spacing throughout child components
- Manages color themes with support for system preference detection
- Controls motion and animations with support for accessibility preferences
- Applies theme classes to the provider element for scoped theming (only when explicitly provided or inherited)
- Provides theme context to all descendant components via React Context
- Handles system theme changes and updates the UI accordingly
- Supports nesting with automatic inheritance from parent providers

When nested, child providers inherit density, theme, and defer settings from their parent unless
explicitly overridden. If no theme is provided and there's no parent context,
no theme class is applied, allowing manually set theme classes to take precedence.
This allows for granular theme control in different sections of the application
while maintaining consistent defaults and not overriding external theme solutions.

## Notes

- If using the ThemeProvider to provide themes for the entire application, you should use the
  `global` prop to apply the theme classes to the `html` element. This will ensure portal'd components are also themed.

## Examples

### Basic Theme Provider

```tsx
<ThemeProvider density="compact">Density-aware children</ThemeProvider>
```

### Basic usage with density control:

```tsx
<ThemeProvider density="compact">
    <Modal>
        <ModalContent>
            <Panel>
                <Tag>Content</Tag>
            </Panel>
        </ModalContent>
    </Modal>
</ThemeProvider>
```

### Full theme management with theme control:

```tsx
<ThemeProvider density="standard" theme="system" motion="system">
    <Header />
    <MainContent />
    <Footer />
</ThemeProvider>
```

### Forcing a specific theme:

```tsx
<ThemeProvider theme="dark">
    <DarkModeSpecificComponent />
</ThemeProvider>
```

### Nested providers with inheritance:

```tsx
<ThemeProvider density="compact" theme="system" motion="system" defer>
  <GlobalLayout>
    {// This provider inherits compact density, system theme, system motion, and defer}
    <ThemeProvider theme="dark" motion="disabled">
      <Sidebar /> {// Uses compact density (inherited) + dark theme + disabled motion + defer (inherited)}
    </ThemeProvider>

    {// This provider inherits all settings from parent}
    <ThemeProvider>
      <MainContent /> {// Uses compact density + system theme + system motion + defer}
    </ThemeProvider>
  </GlobalLayout>
</ThemeProvider>
```

### Respecting external theme classes:

```tsx
{// When no parent context and no explicit theme, allows external classes}
<div className="nv-dark">
  <ThemeProvider>
    <Component /> {// Respects the manually set nv-dark class}
  </ThemeProvider>
</div>

{// But explicit theme still takes precedence}
<div className="nv-dark">
  <ThemeProvider theme="light">
    <Component /> {// Applies nv-light, overriding manual class}
  </ThemeProvider>
</div>
```

## Props

| Prop    | Type                                    | Default                                                 | Description                                                                                                                                                                                                                                                                                                                                                                                                                            |
| ------- | --------------------------------------- | ------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild | `boolean`                               | -                                                       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                                                                                                                                          |
| defer   | `boolean`                               | `false (or inherited from parent)`                      | Whether to defer the theme change until SSR hydration. This can be useful for integration with other theme provider solutions like `next-themes`.                                                                                                                                                                                                                                                                                      |
| density | `"compact" \| "standard" \| "spacious"` | `"standard" (or inherited from parent)`                 | The density variant that controls spacing throughout child components. This affects the density of all child components that would normally have a `spacing` prop. - "spacious": Maximum spacing for accessibility and touch interfaces - "compact": Minimal spacing for information-dense layouts - "standard": Default balanced spacing for general use When nested, inherits from parent ThemeProvider if not specified.            |
| global  | `boolean`                               | `false`                                                 | Whether this provider should manage global theme state by applying classes to DOM elements. When true, theme and density changes are applied to the specified target element (html or body).                                                                                                                                                                                                                                           |
| motion  | `"disabled" \| "system"`                | `"system" (or inherited from parent)`                   | The motion preference for animations and transitions. - "disabled": Force disable all animations and transitions - "system": Automatically follow the user's prefers-reduced-motion preference When nested, inherits from parent ThemeProvider if not specified.                                                                                                                                                                       |
| target  | `"body" \| "html"`                      | `"html"`                                                | The target element to apply theme classes to when global is true. Only used when global=true.                                                                                                                                                                                                                                                                                                                                          |
| theme   | `"light" \| "dark" \| "system"`         | `inherited from parent, or no theme class if no parent` | The theme theme for the application. - "light": Force light theme regardless of system preference - "dark": Force dark theme regardless of system preference - "system": Automatically follow the user's system preference When nested, inherits from parent ThemeProvider if not specified. If no theme is provided and there's no parent context, no theme class is applied, allowing manually set theme classes to take precedence. |
