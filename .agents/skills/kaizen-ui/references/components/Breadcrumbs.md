<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Breadcrumbs

A navigation path showing the user's location within the apps hierarchy, providing a trail back
to parent pages and helping users orient themselves in deep navigation structures. Use concise labels.

Do not use `Breadcrumbs` for root or first-level pages - there's nothing to navigate back to.

## Examples

### Basic Breadcrumbs

```tsx
<Breadcrumbs
    items={[
        { children: <a href="/">Home</a> },
        { children: <a href="/category">Category</a> },
        'Current Page'
    ]}
/>
```

### With Icon

Icons should be used sparingly, but the component supports them.

```tsx
<Breadcrumbs
    items={[
        {
            children: (
                <a className="flex gap-[inherit]" href="/">
                    <Home />
                    Home
                </a>
            )
        },
        { children: <a href="/category">Category</a> },
        {
            children: (
                <>
                    <Identification />
                    Product 123
                </>
            )
        }
    ]}
/>
```

### Composed

```tsx
<BreadcrumbsRoot>
    <BreadcrumbsItem>
        <a href="/">Home</a>
    </BreadcrumbsItem>
    <BreadcrumbsSeparator />
    <BreadcrumbsItem>
        <a href="/category">Category</a>
    </BreadcrumbsItem>
    <BreadcrumbsSeparator />
    <BreadcrumbsItem active>Current Page</BreadcrumbsItem>
</BreadcrumbsRoot>
```

## Props

| Prop          | Type                                  | Default    | Description                                                                                                                   |
| ------------- | ------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------- |
| **items** \*  | `string \| { children: ReactNode }[]` | -          | Array of items to be rendered as breadcrumbs.                                                                                 |
| asChild       | `boolean`                             | -          | When true, renders the immediate child instead of the default element, merging this component's props with the child's props. |
| size          | `"small" \| "medium" \| "large"`      | `"medium"` | Controls the font size of the breadcrumbs. Available in large(16px), medium(14px), and small(12px) font sizes.                |
| slotSeparator | `ReactNode`                           | -          | Replaces the default separator icon with custom rendered content.                                                             |

`* = required prop`
