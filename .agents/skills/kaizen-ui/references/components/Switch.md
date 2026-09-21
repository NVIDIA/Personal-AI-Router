<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Switch

Also known as: Toggle, ToggleSwitch

A switch allows users to quickly switch between two states.

Use a switch to allow users to turn something on or off instantly. Switches are only used with
binary actions with a default setting set. It's commonly used for adjusting settings and
preferences.

## Notes

- Switch is exclusively for immediate-effect toggles (value takes effect as soon as toggled). If the toggle sits inside a form with a submit button, or the value is only applied on save/submit, use Checkbox instead.

## Examples

### Basic Switch

Use when toggling a binary setting that takes effect immediately.

```tsx
<Flex direction="col" gap="density-md">
    <Switch slotLabel="Off" />
    <Switch defaultChecked slotLabel="On" />
</Flex>
```

### Sizes Switch

Use a single size consistently within a group. Default `medium` fits most settings rows; reach for `small` in dense toolbars and `large` for prominent toggles.

```tsx
<div className="flex flex-col items-start gap-4">
    <Switch size="small" slotLabel="Small" />
    <Switch size="medium" slotLabel="Medium" />
    <Switch size="large" slotLabel="Large" />
</div>
```

### Without Label Switch

Use `aria-label` when the switch's purpose is conveyed by surrounding context (e.g. a settings row with its own heading) and a visible label would be redundant.

```tsx
<Switch aria-label="Enable notifications" onCheckedChange={checked => console.log(checked)} />
```

### Label Start Switch

Use when aligning labels at the start for consistency in a settings list.

```tsx
<Switch slotLabel="Dark mode" labelSide="start" />
```

### Disabled Switch

Use when a setting is locked by permissions or an external condition. Pair with a tooltip or helper text explaining why.

```tsx
<Tooltip slotContent="Maintenance mode is unavailable while jobs are running">
    <Switch slotLabel="Maintenance mode" disabled defaultChecked />
</Tooltip>
```

### Form Integration Switch

Switch is a native HTML input element defaulting to `type='checkbox'`. Provide `name` and `value` so the switch participates in native form submission.

```tsx
<Switch name="notifications" value="enabled" slotLabel="Enable notifications" />
```

### As Radio Group Switch

```tsx
<Flex direction="col" gap="density-md">
    <Switch slotLabel="Option 1" type="radio" name="group" />
    <Switch slotLabel="Option 2" type="radio" name="group" />
    <Switch slotLabel="Option 3" type="radio" name="group" />
</Flex>
```

### Composed

Use composed primitives when you need full control over the switch layout or label rendering.

```tsx
<SwitchRoot>
    <SwitchInput aria-label="Enable feature" />
</SwitchRoot>
```

## Props

| Prop            | Type                             | Default    | Description                                                                                                    |
| --------------- | -------------------------------- | ---------- | -------------------------------------------------------------------------------------------------------------- |
| labelSide       | `"start" \| "end"`               | `"end"`    | Which side of the switch to render the label on. Ensure that all switches in a group are consistently aligned. |
| onCheckedChange | `(checked: boolean) => void`     | -          | An event handler that's called when the state of the switch changes.                                           |
| side            | `"start" \| "end"`               | `"end"`    | The side of the switch to render the label on. Ensure that all switches in a group are consistently aligned.   |
| size            | `"small" \| "medium" \| "large"` | `"medium"` | The size of the switch. Use the same size when multiple switches are shown to create a uniform experience.     |
| slotLabel       | `ReactNode`                      | -          | A semantic label for the switch. The location of the label can be changed using the `labelSide` property.      |
