<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Checkbox

Use checkboxes to allow users to select multiple options from a list or to mark a single item as selected

## Examples

### Basic Checkbox

```tsx
<Flex direction="col" gap="3">
    <Checkbox slotLabel="Item" />
    <Checkbox checked="indeterminate" slotLabel="Item" />
    <Checkbox checked slotLabel="Item" />
    <Checkbox checked error slotLabel="Item" />
</Flex>
```

### With Label Checkbox

The label is automatically associated with the input.

```tsx
<Checkbox slotLabel="Accept terms and conditions" />
```

### Without Label Checkbox

Use aria-label to provide a label for the checkbox when no label is provided via `slotLabel` or associating via `htmlFor`.

```tsx
<Checkbox aria-label="Accept terms and conditions" />
```

### With Callback Checkbox

```tsx
<Checkbox onCheckedChange={checked => console.log(checked)} slotLabel="Toggle me" />
```

### Indeterminate Checkbox

```tsx
<Checkbox checked="indeterminate" slotLabel="Select all" />
```

### Disabled Checkbox

Should generally be paired with an accompanying tooltip or description to explain why it's disabled.

```tsx
<Checkbox disabled slotLabel="Unavailable option" />
```

### Error Checkbox

Renders in an error state.

```tsx
<Checkbox error slotLabel="Required field" />
```

### Label Left Checkbox

```tsx
<Checkbox labelSide="left" slotLabel="Left-side label" />
```

### Composed

```tsx
<CheckboxRoot>
    <CheckboxInput defaultChecked id="my-checkbox" />
    <Label htmlFor="my-checkbox">Checkbox Label</Label>
</CheckboxRoot>
```

## Props

| Prop            | Type                                                  | Default   | Description                                                                                                                                                                         |
| --------------- | ----------------------------------------------------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| checked         | `false \| true \| "indeterminate"`                    | -         | The controlled checked state of the checkbox. Must be used in conjunction with `onCheckedChange`.                                                                                   |
| defaultChecked  | `boolean`                                             | -         | The checked state of the checkbox when it is initially rendered. Use when you do not need to control its checked state.                                                             |
| disabled        | `boolean`                                             | -         | If true. Puts the checkbox into a disabled state.                                                                                                                                   |
| error           | `boolean`                                             | -         | If true. Shows the checkbox in its error state.                                                                                                                                     |
| labelSide       | `"right" \| "left"`                                   | `"right"` | The side of the label to render the checkbox on.                                                                                                                                    |
| onCheckedChange | `(checked: false \| true \| "indeterminate") => void` | -         | Event handler called when the checked state of the checkbox changes.                                                                                                                |
| slotLabel       | `ReactNode`                                           | -         | A semantic label for the checkbox. Goes on either the right or left side of the checkbox can be changed using the `labelSide` property. Automatically associated with the checkbox. |
