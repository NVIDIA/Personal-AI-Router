<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Skeleton

A skeleton component is a placeholder that mimics the layout of content while it loads, giving users a sense of the structure and reducing perceived wait time.
Always try and make the skeleton components as simular in size and shape to the dynamic content as you can

## Examples

### Kinds Skeleton

Pick a `kind` to match the shape of the content being loaded: `line` (default) for single-line text, `pill` for tags or badges, and `circle` for avatars or icons.

```tsx
<Stack gap="density-md">
    <Skeleton kind="circle" />
    <Skeleton kind="pill" />
    <Skeleton kind="line" />
    <Skeleton kind="line" />
</Stack>
```

### Static Skeleton

Set `animated={false}` when the skeleton flashes briefly or sits inside a surface that already conveys a loading state, where the pulse would be visual noise.

```tsx
<Skeleton kind="line" animated={false} />
```

### Inline Text Skeleton

By default, the skeleton will inherit the surrounding font size — ideal for swapping in for a heading or label while its data resolves.

```tsx
<Text kind="title/lg">
    <Skeleton />
</Text>
```

### Sized Skeleton

Override the default dimensions with `className` or `style` so the skeleton matches the exact size of the component it stands in for, like a large `Avatar` or icon.

```tsx
<Skeleton kind="circle" className="size-12" />
```

### Content Block Skeleton

Compose multiple skeleton shapes inside layout primitives to mirror the rough size and rhythm of a content block so the page does not jump when real data loads in.

```tsx
<Stack gap="density-lg" style={{ maxWidth: '320px' }}>
    <Skeleton kind="circle" />
    <Skeleton kind="line" style={{ width: '66%' }} />
    <Stack gap="density-sm">
        <Skeleton kind="line" />
        <Skeleton kind="line" />
        <Skeleton kind="line" />
    </Stack>
    <Flex gap="density-sm" wrap="wrap">
        <Skeleton kind="pill" />
        <Skeleton kind="pill" />
    </Flex>
</Stack>
```

## Props

| Prop     | Type                           | Default  | Description                                                                                                                                                                                                                                                                                                                |
| -------- | ------------------------------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| animated | `boolean`                      | `true`   | If true, the Skeleton will pulse. All skeletons will pulse at the same rhythm                                                                                                                                                                                                                                              |
| asChild  | `boolean`                      | -        | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                              |
| kind     | `"circle" \| "line" \| "pill"` | `"line"` | The primary kind of skeleton to build a UI that best matches your component. - `line` - a horizontal line with hard corners and a 1.3em height, allowing it to size itself to the container font size dynamically - `pill` - a pill shape with a 64px width and rounded corners - `circle` - a circle with a 32px diameter |
