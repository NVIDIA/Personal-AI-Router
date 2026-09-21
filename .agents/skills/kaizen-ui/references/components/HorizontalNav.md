<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# HorizontalNav

The HorizontalNav component is a customizable navigation bar designed to display a list of links in a horizontal layout.
It allows users to navigate between different sections of an application or website.

## Notes

- Never render nav items as disabled or aria-disabled for role-gated destinations. Remove inaccessible items from the items array entirely.

## Examples

### Basic Horizontal Nav

Use for top-level site navigation where users move between major sections.

```tsx
<HorizontalNav
    defaultValue="home"
    items={[
        { value: 'home', href: '/', children: 'Home' },
        { value: 'about', href: '/about', children: 'About' },
        { value: 'contact', href: '/contact', children: 'Contact' }
    ]}
/>
```

### With Icons Horizontal Nav

Use when icons help users quickly identify navigation destinations.

```tsx
<HorizontalNav
    defaultValue="home"
    items={[
        {
            value: 'home',
            href: '/',
            children: (
                <>
                    <Icon /> Home
                </>
            )
        },
        {
            value: 'about',
            href: '/about',
            children: (
                <>
                    <Icon /> About
                </>
            )
        }
    ]}
/>
```

### With Button Item Horizontal Nav

Use `asChild: true` on an item to render a non-anchor element (e.g. a button that opens a modal) while keeping it in the tab order.

```tsx
<HorizontalNav
    defaultValue="home"
    items={[
        { value: 'home', href: '/', children: 'Home' },
        { value: 'about', href: '/about', children: 'About' },
        {
            value: 'settings',
            asChild: true,
            children: <button type="button">Settings</button>
        }
    ]}
/>
```

### With Custom Link Horizontal Nav

Use `renderLink` to swap the underlying anchor for a framework router component (Next.js `Link`, React Router `Link`, etc.) so navigation stays client-side.

```tsx
<HorizontalNav
    defaultValue="home"
    items={[
        { value: 'home', href: '/', children: 'Home' },
        { value: 'about', href: '/about', children: 'About' }
    ]}
    renderLink={item => <Link {...item} />}
/>
```

### With Disabled Item Horizontal Nav

Use when a nav destination exists but is temporarily unavailable.

```tsx
<HorizontalNav
    defaultValue="home"
    items={[
        { value: 'home', href: '/', children: 'Home' },
        {
            value: 'settings',
            href: '/settings',
            children: 'Settings',
            disabled: true
        },
        { value: 'about', href: '/about', children: 'About' }
    ]}
/>
```

### Composed

```tsx
<HorizontalNavRoot defaultValue="home">
    <HorizontalNavList>
        <HorizontalNavLink value="home" href="/">
            Home
        </HorizontalNavLink>
        <HorizontalNavLink value="about" href="/about">
            About
        </HorizontalNavLink>
        <HorizontalNavLink value="contact" href="/contact">
            Contact
        </HorizontalNavLink>
    </HorizontalNavList>
</HorizontalNavRoot>
```

## Props

| Prop          | Type                                                                                                                                          | Default | Description                                                                                                                                                                          |
| ------------- | --------------------------------------------------------------------------------------------------------------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **items** \*  | `{ asChild: boolean; children: ReactNode; disabled: boolean; href: string; target: string; rel: string; value: string }[]`                    | -       | Array of navigation items to render in the horizontal nav                                                                                                                            |
| asChild       | `boolean`                                                                                                                                     | -       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                        |
| defaultValue  | `string`                                                                                                                                      | -       | The value of the horizontal nav when initially rendered. Use when you do not need to control the state of the horizontal nav.                                                        |
| onValueChange | `(value: string) => void`                                                                                                                     | -       | Callback fired when the value of the horizontal nav changes.                                                                                                                         |
| renderLink    | `(item: { asChild: boolean; children: ReactNode; disabled: boolean; href: string; target: string; rel: string; value: string }) => ReactNode` | -       | Custom renderer for individual navigation links. Useful for rendering a framework specific link component.                                                                           |
| value         | `string`                                                                                                                                      | -       | The controlled value of the horizontal nav. Must be used in conjunction with `onValueChange`. Use this when you need to control the value of the horizontal nav with external state. |

`* = required prop`
