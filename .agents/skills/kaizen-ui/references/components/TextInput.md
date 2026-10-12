<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# TextInput

This component is a composition of the `InputShell` and `input` components. The `ref` and HTML attributes are applied to the inner `input` component.

## Notes

- When used as a search input, always include a leading magnifying-glass icon via slotStart for affordance.

## Examples

### Basic Text Input

```tsx
<Flex direction="col" gap="density-md">
    <TextInput defaultValue="Hello, world!" />
    <TextInput type="time" />
</Flex>
```

### With Slots Text Input

Use slotStart for affordance icons (e.g. a magnifying glass for search) and slotEnd for trailing hints like result counts or unit labels.

```tsx
<TextInput slotStart={<Bell />} slotEnd={<span>(7)</span>} defaultValue="admin" />
```

### Dismissible Text Input

Use when the user needs a quick way to clear the field, such as search inputs.

```tsx
<TextInput dismissible defaultValue="Clearable value" />
```

### Read Only Text Input

Use readOnly (instead of disabled) when the value should remain visible, focusable, and copyable but not editable — common in summary or review screens.

```tsx
<TextInput readOnly value="Read-only value" />
```

### Disabled Text Input

Use when the field value is locked by external conditions and cannot be changed.

```tsx
<TextInput disabled defaultValue="Cannot edit" />
```

### Size Text Input

Match the input size to surrounding controls — small for dense tables/toolbars, large for hero search bars or onboarding forms.

```tsx
<Stack gap="density-md">
    <TextInput size="small" placeholder="Small" />
    <TextInput size="medium" placeholder="Medium" />
    <TextInput size="large" placeholder="Large" />
</Stack>
```

### Status Text Input

Set `status` to drive validation styling manually — `error` for failed validation or `success` to confirm a passing value. Reach for `withValidation` when native HTML constraints can drive the state automatically.

```tsx
<Stack gap="density-md">
    <TextInput status="error" defaultValue="invalid-email" />
    <TextInput status="success" defaultValue="valid@example.com" />
</Stack>
```

### With Validation Text Input

Use withValidation to drive success/error styling automatically from native HTML constraints (`required`, `pattern`, `type="email"`, etc.) via `:user-valid` and `:user-invalid` — no controlled state needed.

```tsx
<TextInput withValidation required type="email" placeholder="name@example.com" />
```

### Date Text Input

Use type="date" or type="datetime-local" to render a calendar button that opens the browser's native picker via `showPicker()`.

```tsx
<Stack gap="density-md">
    <TextInput type="date" />
    <TextInput type="datetime-local" />
</Stack>
```

### Time Text Input

Use for time selection with a native picker button. Set step={60} to hide seconds.

```tsx
<TextInput type="time" step={60} />
```

### With Form Field Text Input

Wrap in FormField to attach a label, helper text, and required indicator. FormField pipes `id`, `name`, `required`, and validation status into the input via context.

```tsx
<FormField
    name="username"
    slotLabel="Username"
    slotHelp="3–20 characters, letters and numbers only."
    required
>
    <TextInput placeholder="Enter your username" />
</FormField>
```

### Controlled Text Input

Use controlled mode when state lives outside the input — e.g. when syncing with form libraries or URL state.

```tsx
;() => {
    const [value, setValue] = useState('')
    return <TextInput value={value} onValueChange={setValue} placeholder="Type something" />
}
```

### Debounced Text Input

Combine with `useDebounce` to throttle expensive work like API calls or filtering driven by the input value.

```tsx
;() => {
    const [value, setValue] = useState('')
    const debounced = useDebounce(value, 500)
    return (
        <Stack gap="density-md">
            <TextInput value={value} onValueChange={setValue} placeholder="Search..." />
            <Text>Debounced: {debounced}</Text>
        </Stack>
    )
}
```

### Composed

Drop down to InputShell + a raw `<input>` when you need full control over slot content, ARIA, or non-standard layouts that TextInput's API doesn't expose.

```tsx
<InputShell>
    <Bell />
    <input type="text" placeholder="Composed text input" />
</InputShell>
```

## Props

| Prop                 | Type                                                                  | Default        | Description                                                                                                                                                                                                                                                                                       |
| -------------------- | --------------------------------------------------------------------- | -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild              | `boolean`                                                             | -              | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                     |
| disableFocusRedirect | `boolean`                                                             | `false`        | When true, the input will not redirect focus to the input when clicked.                                                                                                                                                                                                                           |
| dismissible          | `boolean`                                                             | -              | When set to `true`, the TextInput will render a dismiss button that will clear and focus the input on click                                                                                                                                                                                       |
| kind                 | `"flat" \| "floating"`                                                | `"flat"`       | The kind of input to render. - "flat" - renders with a border and background - "floating" - renders borderless with no background                                                                                                                                                                 |
| layout               | `"horizontal" \| "vertical"`                                          | `"horizontal"` | The layout direction for slotted content. - "horizontal" - children are laid out in a row (default) - "vertical" - children are laid out in a column with auto height and block padding                                                                                                           |
| onDismiss            | `(event: React.MouseEvent<HTMLButtonElement, MouseEvent>) => void`    | -              | Handler to override the default onDismiss functionality                                                                                                                                                                                                                                           |
| onValueChange        | `(value: string, event: React.ChangeEvent<HTMLInputElement>) => void` | -              | Convenience callback, provides the value as the first argument                                                                                                                                                                                                                                    |
| size                 | `"small" \| "medium" \| "large"`                                      | `"medium"`     | The size of the input to render. Available sizes are "small", "medium", and "large".                                                                                                                                                                                                              |
| slotEnd              | `ReactNode`                                                           | -              | Content to render on the end side of the input. By default this is the right, and `layout="vertical"` will have this on the bottom.                                                                                                                                                               |
| slotStart            | `ReactNode`                                                           | -              | Content to render on the start side of the input. By default this is the left, and `layout="vertical"` will have this on the top. For `type="time"`, `type="date"`, and `type="datetime-local"`, a picker button is always rendered first; when `slotStart` is set, it renders after that button. |
| status               | `"error" \| "success"`                                                | -              | The status of the input. Use `withValidation` to automatically apply success/error states based on `:user-valid` and `:user-invalid` pseudo classes.                                                                                                                                              |
| value                | `string`                                                              | -              | The value of the text input. Must be used in combination with `onChange/onValueChange` to ensure the value can be updated, otherwise this becomes read only.                                                                                                                                      |
| withValidation       | `boolean`                                                             | -              | When true, the input will automatically display success/error state based on `:user-valid` and `:user-invalid` styles                                                                                                                                                                             |
