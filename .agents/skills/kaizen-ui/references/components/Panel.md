<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Panel

A panel is a simple container used to wrap content and actions for a given component in a
card-like format with a background, border, and padding.

## Notes

- Panel manages its own internal padding. Do not add Tailwind padding classes (p-4, p-5, etc.) to className — they stack with the internal padding and create visual doubling.

## Examples

### Basic Panel

Use to wrap related content and actions in a card-like container with a background, border, and padding.

```tsx
<Panel slotHeading="Panel Title">Panel content goes here.</Panel>
```

### With Icon Panel

Use when an icon helps identify the panel's purpose at a glance.

```tsx
<Panel slotHeading="Configuration" slotIcon={<Icon />}>
    Manage your settings and preferences.
</Panel>
```

### With Footer Panel

Use when the panel has actions that apply to its content, such as save or cancel buttons.

```tsx
<Panel
    slotHeading="Job Details"
    slotIcon={<Icon />}
    slotFooter={
        <>
            <Button kind="secondary">Cancel</Button>
            <Button color="brand">Save</Button>
        </>
    }
>
    Fill in the details for your new job.
</Panel>
```

### High Elevation Panel

Use when you need increased contrast between the panel and the page background, such as in layered UIs.

```tsx
<Panel slotHeading="Overlay Panel" elevation="high">
    This panel uses a raised background for increased contrast.
</Panel>
```

### Compact Density Panel

Use to override panel spacing locally when a single panel needs to be denser than the surrounding ThemeProvider density.

```tsx
<Panel
    density="compact"
    slotHeading="Storage"
    slotIcon={<Icon />}
    slotFooter={<Button color="brand">Save</Button>}
>
    Tightens internal padding and footer spacing for dense layouts.
</Panel>
```

### Content Only Panel

Use for simple content containers where a heading is unnecessary.

```tsx
<Panel>Content without a heading or footer.</Panel>
```

### Composed

```tsx
<PanelRoot elevation="mid">
    <PanelHeader>
        <PanelIcon>
            <Icon />
        </PanelIcon>
        <PanelHeading>Panel Title</PanelHeading>
    </PanelHeader>
    <PanelContent>Panel content goes here.</PanelContent>
    <PanelFooter>
        <Button color="brand">Action</Button>
    </PanelFooter>
</PanelRoot>
```

## Props

| Prop        | Type                                    | Default | Description                                                                                                                                                                                                                                                                                                                                                                                                                          |
| ----------- | --------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| asChild     | `boolean`                               | -       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                                                                                                                                        |
| density     | `"compact" \| "standard" \| "spacious"` | -       | The density of the panel                                                                                                                                                                                                                                                                                                                                                                                                             |
| elevation   | `"high" \| "low" \| "higher" \| "mid"`  | `"mid"` | The elevation of the panel. This changes the background color in dark mode to indicate hierarchy. In light mode background color only changes for `low` elevation. Set this if you want increased contrast between the panel and the page background. - `high`: --background-color-surface-raised - `higher`: --background-color-surface-overlay - `mid`: --background-color-surface-base - `low`: --background-color-surface-sunken |
| slotFooter  | `ReactNode`                             | -       | The slot for the footer of the panel. Automatically handles spacing and aligns content to the end of the container.                                                                                                                                                                                                                                                                                                                  |
| slotHeading | `ReactNode`                             | -       | The slot for the heading of the panel. This is rendered at the top of the panel with `label/bold/xl` typography styles.                                                                                                                                                                                                                                                                                                              |
| slotIcon    | `ReactNode`                             | -       | Icon to be displayed in the panel. This is rendered to the left of the heading if provided and handles sizing and color for the icon automatically.                                                                                                                                                                                                                                                                                  |
