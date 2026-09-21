<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Avatar

Use an avatar whenever it's helpful to quickly identify a user. Prioritize images over text to improve clarity.
Provide the user's initials as a fallback when no image is available.

## Examples

### Basic Avatar

```tsx
<Avatar fallback="JH" alt="Jensen Huang" size="xlarge" src="jhh.jpg" />
```

### With Name

```tsx
<div className="flex items-center gap-2">
    <Avatar
        fallback="JH"
        // use empty alt to avoid redundancy with name text
        alt=""
        src="jhh.jpg"
    />
    <Text kind="label/bold/sm">Jensen Huang</Text>
</div>
```

### Interactive Avatar

Sometimes Avatars are used as triggers - in that case use the `interactive` prop so the Avatar has hover states.

```tsx
<Avatar fallback="JH" alt="Jensen Huang" src="jhh.jpg" interactive />
```

### As Link Or Button

To render an Avatar as a link or button, use the asChild prop on AvatarRoot to render it as its child element.

```tsx
<AvatarRoot asChild interactive size="large">
    <a
        // or use a button element, depending on the use case
        href="https://www.nvidia.com/jensen-huang/"
        rel="noreferrer noopener"
        target="_blank"
    >
        <AvatarImage alt="Jensen Huang" src="jhh.jpg" />
        <AvatarFallback>JH</AvatarFallback>
    </a>
</AvatarRoot>
```

### Composed

```tsx
<AvatarRoot size="xxlarge">
    <AvatarImage alt="Jensen Huang" src="jhh.jpg" />
    <AvatarFallback>JH</AvatarFallback>
</AvatarRoot>
```

## Props

| Prop            | Type                                                      | Default     | Description                                                                                                                                                                                                                           |
| --------------- | --------------------------------------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **fallback** \* | `ReactNode`                                               | -           | Fallback text displayed in the event of no image source, or the image fails to load. Always provide initials or an icon fallback.                                                                                                     |
| alt             | `string`                                                  | -           | Alt text for the Avatar image. You should always provide this. For user profiles, use the user's name (not just the initials). If the user's name is already displayed though, use an empty string to avoid screen reader redundancy. |
| interactive     | `boolean`                                                 | -           | If true, the avatar will have hover and active interactions and pointer styling. If the `onClick` prop is set, this defaults to true.                                                                                                 |
| kind            | `"outline" \| "solid"`                                    | `"outline"` | The style of the avatar, either "outline" or "solid" (no outline). Use "outline" for lighter-weight avatars in dense layouts.                                                                                                         |
| size            | `"small" \| "medium" \| "large" \| "xlarge" \| "xxlarge"` | `"medium"`  | The size of the avatar. Use "medium" for most cases. "small" in lists/tables. "large" / "xlarge" / "xxlarge" for profile detail pages, depending on the context.                                                                      |
| src             | `string`                                                  | -           | The source of the image for the users Avatar                                                                                                                                                                                          |

`* = required prop`
