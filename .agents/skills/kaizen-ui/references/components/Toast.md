<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Toast

Use a toast to display small messages without disrupting a user's experience. Toasts are commonly
used to provide non-critical, contextual feedback following a user's action or process. They're
low-emphasis and appear temporarily as an overlay.

## Notes

- Do not use Toast for errors, warnings, or any message requiring user action — Toast should auto-dismiss. Use Notification or Banner instead for actionable messages.

## Examples

### Statuses Toast

Set `status` to convey severity: `info` for neutral confirmation, `success` for completed actions, `warning` to caution about a degraded condition, and `error` for failures that need corrective action.

```tsx
<Flex direction="col" gap="density-md">
    <Toast status="info">Operation completed</Toast>
    <Toast status="success">Changes saved</Toast>
    <Toast status="warning">Connection unstable</Toast>
    <Toast status="error">Upload failed</Toast>
</Flex>
```

### Dismissible Toast

Use when the user should be able to manually dismiss the toast before it auto-hides.

```tsx
<Toast status="info" onClose={handleClose}>
    Session expiring soon
</Toast>
```

### With Action Toast

Use when the toast should offer an inline action for the user to respond to the message.

```tsx
<Toast
    status="error"
    onClose={handleClose}
    slotAction={
        <Button kind="tertiary" size="small">
            Retry
        </Button>
    }
>
    Upload failed
</Toast>
```

### Working Toast

Use to indicate that a background process is actively running.

```tsx
<Toast status="working">Processing request</Toast>
```

### With Custom Icon Toast

Use `slotIcon` to override the default status icon when a domain-specific glyph communicates the message better.

```tsx
<Toast status="success" slotIcon={<Bell />}>
    Reminder scheduled
</Toast>
```

### With Title Toast

Set the native `title` attribute so the full message is available on hover when the toast text is long enough to truncate.

```tsx
<Toast status="info" title="Deployment finished in 3m 24s across 4 regions">
    Deployment finished in 3m 24s across 4 regions
</Toast>
```

### Composed

Use composed primitives when you need full control over the toast layout, icon, and actions.

```tsx
<ToastRoot status="success">
    <ToastContent>
        <ToastIcon status="success" />
        <ToastText>Composed toast message</ToastText>
    </ToastContent>
    <ToastActions>
        <Button kind="tertiary" size="small">
            Undo
        </Button>
    </ToastActions>
</ToastRoot>
```

## Props

| Prop       | Type                                                                    | Default  | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| ---------- | ----------------------------------------------------------------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild    | `boolean`                                                               | -        | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                                                                                                                                                                                                                          |
| children   | `ReactNode`                                                             | -        | The message to display in the toast. Toast messages should use brief and direct text, no more than 4 words.                                                                                                                                                                                                                                                                                                                                                                                                            |
| onClose    | `() => void`                                                            | -        | A callback to be called when the toast is closed.                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| slotAction | `ReactNode`                                                             | -        | Slot for the action to render in the toast. Should consist of a `size="small"` button.                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| slotIcon   | `ReactNode`                                                             | -        | Slot for the icon to render in the toast.                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| status     | `"neutral" \| "error" \| "warning" \| "success" \| "info" \| "working"` | `"info"` | The status of the toast. The toast can be either info, success, warning, error, neutral, or working. - `info`: Provides additional information that may require action - `success`: Confirms to the user that they have completed an action or task - `warning`: Cautions the user on something urgent to avoid an issue - `error`: Communicates a critical issue that needs attention and provides a next step - `neutral`: Provides general information - `working`: Communicates a process is actively taking place |
