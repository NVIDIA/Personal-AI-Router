<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# AppBar

Also known as: Masthead, Navbar, Navigation Bar

The top-level masthead for the application. Can contain the NVIDIA logo(or custom branding), navigation,
and global actions (user menu, settings). Every application has exactly one AppBar.

## Examples

### Simple Example

```tsx
<AppBar
    slotStart={
        <>
            <AppBarExpanderButton />
            <span>Product</span>
        </>
    }
/>
```

### Full Example

Note that "AppBarLogo" is included with `@kui/foundations-react` or can come from `@kui-contrib/brand-logo` if you are using that package.

```tsx
<AppBar
    slotStart={
        <>
            <AppBarExpanderButton onClick={() => console.log('toggle')} />
            <AppBarLogo />
            <Anchor kind="standalone" href="/" textKind="inherit">
                Product Name
            </Anchor>
        </>
    }
    slotEnd={<Avatar interactive fallback="NV" />}
>
    <HorizontalNav
        defaultValue="home"
        items={[
            { value: 'home', href: '/', children: 'Home' },
            { value: 'about', href: '/about', children: 'About' },
            { value: 'contact', href: '/contact', children: 'Contact' }
        ]}
    />
</AppBar>
```

### With Centered Content

```tsx
<AppBar>
    <div className="mx-auto">
        <HorizontalNav
            defaultValue="home"
            items={[
                { value: 'home', href: '/', children: 'Home' },
                { value: 'about', href: '/about', children: 'About' },
                { value: 'contact', href: '/contact', children: 'Contact' }
            ]}
        />
    </div>
</AppBar>
```

## Props

| Prop      | Type        | Default | Description                                                                                                                                                                                                                                                                                       |
| --------- | ----------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| children  | `ReactNode` | -       | Renders in the AppBar between the left and right slots. By default this is left aligned. This is where you would place your app's navigation using HorizontalNav - do not include more than 7 links for proper UX. For more links, you should use VerticalNav in your app and no links in AppBar. |
| slotEnd   | `ReactNode` | -       | The slot for the end(right) side of the AppBar. Intended to contain global actions (user menu, settings).                                                                                                                                                                                         |
| slotStart | `ReactNode` | -       | The slot for the start(left) side of the AppBar. Intended to contain the brand area and product name.                                                                                                                                                                                             |
