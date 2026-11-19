<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Notification

A notification component that displays important messages and alerts to users.
It provides a flexible layout with support for icons, headings, subheadings, close buttons and footers.

The component uses a grid-based layout through NotificationContent to organize its elements:

- Icon on the left (optional)
- Header content in the middle containing heading and subheading
- Close button on the right (optional)
- Footer below (optional)

## Examples

### Basic Notification

Use for persistent informational messages that provide context the user may need to act on.

```tsx
<Notification status="info" slotSubheading="Check your inbox for details.">
    New message received
</Notification>
```

### Dismissible Notification

Pass `onClose` to render the dismiss button — use whenever the user should be able to clear the notification themselves.

```tsx
<Notification
    status="success"
    slotSubheading="Your model is ready for inference."
    onClose={handleClose}
>
    Deployment complete
</Notification>
```

### Statuses Notification

Set `status` to convey severity: `info` for context, `success` for confirmation, `warning` for caution, and `error` for critical failures that need a next step.

```tsx
<Flex direction="col" gap="density-md">
    <Notification status="info" slotSubheading="Added 50K labeled images to the dataset.">
        New training data available
    </Notification>
    <Notification
        status="success"
        slotSubheading="Inference endpoint active at api.nvidia.com/v1/chat."
    >
        Model deployed successfully
    </Notification>
    <Notification
        status="warning"
        slotSubheading="GPU 0: 92% memory utilization — consider reducing batch size."
    >
        High GPU memory usage detected
    </Notification>
    <Notification
        status="error"
        slotSubheading="Service temporarily unavailable — engineers investigating."
    >
        Inference endpoint unavailable
    </Notification>
</Flex>
```

### With Footer Notification

Pass `slotFooter` with action buttons when the notification needs an inline follow-up action like view, retry, or dismiss.

```tsx
<Notification
    status="warning"
    slotSubheading="3 resources are over budget this month."
    slotFooter={
        <>
            <Button kind="secondary" size="small">
                View Details
            </Button>
            <Button kind="tertiary" size="small">
                Dismiss
            </Button>
        </>
    }
>
    Budget alert
</Notification>
```

### With Icon Notification

Override `slotIcon` when a domain-specific glyph communicates the message better than the default status icon.

```tsx
<Notification
    status="info"
    slotIcon={<Bell />}
    slotSubheading="Version 2.1.0 is available."
    onClose={handleClose}
>
    Update available
</Notification>
```

### Inline Notification

Use `kind="inline"` when the notification renders alongside content (e.g. inline form validation) rather than as a stacked toast-style block.

```tsx
<Notification kind="inline" status="error" slotSubheading="This field is required.">
    Validation error
</Notification>
```

### Composed

Use composed primitives when you need full control over the notification grid layout and element placement.

```tsx
<NotificationRoot status="success">
    <NotificationContent>
        <NotificationIcon status="success" />
        <NotificationHeader>
            <NotificationHeading>Composed notification</NotificationHeading>
            <NotificationSubheading>
                Using primitives for full layout control.
            </NotificationSubheading>
        </NotificationHeader>
        <NotificationFooter>
            <Button kind="secondary" size="small">
                View
            </Button>
        </NotificationFooter>
    </NotificationContent>
</NotificationRoot>
```

## Props

| Prop           | Type                                                                                                                           | Default                 | Description                                                                                                                                                                                                                                                                                                                                                               |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------ | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| children       | `ReactNode`                                                                                                                    | -                       | The main heading text or element to display in the notification. This is required and serves as the primary message.                                                                                                                                                                                                                                                      |
| kind           | `"inline" \| "stacked"`                                                                                                        | `"stacked"`             | Notification variants. - `stacked`: Displays a notification with a header, subheading, and footer stacked vertically - `inline`: Displays a notification with a header, subheading, and footer in a single line                                                                                                                                                           |
| onClose        | `() => void`                                                                                                                   | -                       | Optional callback function that is called when the close button is clicked. If provided, displays a close button in the notification.                                                                                                                                                                                                                                     |
| ref            | `((instance: HTMLDivElement) => void \| (() => void \| { [UNDEFINED_VOID_ONLY]: never; })) \| React.RefObject<HTMLDivElement>` | -                       | Allows getting a ref to the component instance. Once the component unmounts, React will set `ref.current` to `null` (or call the ref with `null` if you passed a callback ref).                                                                                                                                                                                           |
| slotCloseIcon  | `ReactNode`                                                                                                                    | `<Icon name="close" />` | Optional custom close icon element to replace the default close button icon. Only displayed when onClose handler is provided.                                                                                                                                                                                                                                             |
| slotFooter     | `ReactNode`                                                                                                                    | -                       | Optional footer content to display at the bottom of the notification. Can contain actions, links or additional information.                                                                                                                                                                                                                                               |
| slotIcon       | `ReactNode`                                                                                                                    | -                       | Optional icon element to display on the left side of the notification. Typically used to indicate the notification type or status.                                                                                                                                                                                                                                        |
| slotSubheading | `ReactNode`                                                                                                                    | -                       | Optional subheading text or element to display below the main heading. Provides additional context or details about the notification.                                                                                                                                                                                                                                     |
| status         | `"error" \| "warning" \| "success" \| "info"`                                                                                  | `"info"`                | Notifications include 4 statuses that convey semantic meaning: - `info`: Provides additional information that may require action - `success`: Confirms to the user that they have completed an action or task - `warning`: Cautions the user on something urgent to avoid an issue - `error`: Communicates a critical issue that needs attention and provides a next step |
