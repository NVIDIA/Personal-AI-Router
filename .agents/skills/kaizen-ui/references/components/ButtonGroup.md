<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# ButtonGroup

Use a Button Group to offer a set of choices that are related. It's recommended to show no more
than 4 actions. Also consider a Segmented Control when actions are mutually exclusive and must have
one active at all times.

## Examples

### Basic Button Group

Use when offering a set of related choices. Recommended to show no more than 4 actions.

```tsx
<ButtonGroup
    items={[{ children: 'Start Training' }, { children: 'Validate' }, { children: 'Stop' }]}
/>
```

### With Children Button Group

Use the children API when you need more control over the button group, such as adding dropdowns. Buttons rendered within will use the ButtonGroup's size/kind/color.

```tsx
<ButtonGroup size="large" color="brand">
    <Button>Start Training</Button>
    <Button>Stop</Button>
    <Dropdown
        // no children, so Dropdown renders as chevron only icon so aria-label is required
        aria-label="Validate or See Details"
        items={['Validate', 'See Details']}
    />
</ButtonGroup>
```

### With Icon Button Group

```tsx
<ButtonGroup
    items={[
        { children: 'Start Training' },
        { children: 'Validate' },
        { children: <SettingsIcon />, key: 'settings', 'aria-label': 'Settings' }
    ]}
/>
```

### Disabled Button Group

Use when the entire group of actions is unavailable due to a precondition.

```tsx
<ButtonGroup
    disabled
    items={[{ children: 'Start Training' }, { children: 'Validate' }, { children: 'Stop' }]}
/>
```

### Disabled Individual Button

You can also disable individual buttons in the group by passing a `disabled` prop to the button. We recommend using children in this case so you can provide a tooltip explaining why the button is disabled.

```tsx
<ButtonGroup>
    <Tooltip disabled={!trainingDisabled} slotContent="Queue is full">
        <Button disabled={trainingDisabled}>Train Model</Button>
    </Tooltip>
    <Button color="danger">Delete Dataset</Button>
</ButtonGroup>
```

## Props

| Prop      | Type                                                                                                               | Default     | Description                                                                                                                   |
| --------- | ------------------------------------------------------------------------------------------------------------------ | ----------- | ----------------------------------------------------------------------------------------------------------------------------- |
| asChild   | `boolean`                                                                                                          | -           | When true, renders the immediate child instead of the default element, merging this component's props with the child's props. |
| color     | `"brand" \| "neutral" \| "danger"`                                                                                 | -           | The color of the buttons in the group. Overrides individual button colors.                                                    |
| disabled  | `boolean`                                                                                                          | -           | Disables all buttons in the group.                                                                                            |
| groupKind | `"gap" \| "flush" \| "border"`                                                                                     | -           | The internal "Group['kind']" to render. This manages how buttons are separated.                                               |
| items     | `ButtonProps & { children: string; key?: string; } \| ButtonProps & { children: React.ReactNode; key: string; }[]` | -           | The items to render in the button group. Either use this or the `children` prop.                                              |
| kind      | `"primary" \| "secondary" \| "tertiary"`                                                                           | `"primary"` | The kind of buttons in the group. Overrides individual button kinds.                                                          |
| size      | `"small" \| "tiny" \| "medium" \| "large"`                                                                         | -           | The size of the individual buttons in the group. Overrides individual button sizes.                                           |
