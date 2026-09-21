<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# VerticalNav

The VerticalNav component provides a vertical navigation menu that can handle
both single links and nested link groups using native HTML `<details>`/`<summary>`.

## Notes

- VerticalNav supports a maximum of two levels (parent -> child). Do not add a third nesting level; flatten the structure or use breadcrumbs for deeper hierarchies.

- Omit slotIcon on nav items by default. Only include icons when the design explicitly calls for them.

- Never render nav items as disabled or aria-disabled for role-gated destinations. Remove inaccessible items from the items array entirely.

## Examples

### Basic Vertical Nav

Use for sidebar navigation with a flat list of destinations.

```tsx
<VerticalNav
    items={[
        { id: 'home', children: 'Home', href: '/' },
        {
            id: 'settings',
            children: 'Settings',
            subItems: [
                {
                    id: 'profile',
                    active: true,
                    children: 'Profile',
                    href: '/settings/profile'
                },
                {
                    id: 'account',
                    children: 'Account',
                    href: '/settings/account'
                }
            ]
        }
    ]}
/>
```

### Default Closed Vertical Nav

Set defaultOpen: false on a group to start collapsed — use when the nav has many sections and you want to reduce visual noise on first load.

```tsx
<VerticalNav
    items={[
        {
            id: 'datacenter',
            children: 'Data Center',
            defaultOpen: false,
            slotIcon: <Icon />,
            subItems: [
                {
                    id: 'datacenter-home',
                    children: 'Home',
                    href: '/data-center'
                },
                {
                    id: 'datacenter-products',
                    children: 'Products',
                    href: '/data-center/products'
                }
            ]
        },
        {
            id: 'omniverse',
            children: 'Omniverse',
            defaultOpen: false,
            slotIcon: <Icon />,
            subItems: [
                {
                    id: 'omniverse-apps',
                    children: 'Apps',
                    href: '/omniverse/apps'
                }
            ]
        }
    ]}
/>
```

### Render Link Vertical Nav

Use renderLink to integrate a framework router (Next.js Link, React Router NavLink, etc.) — a single renderer is applied to both primary and secondary items via the asChild pattern, so styling stays consistent.

```tsx
<VerticalNav
    items={[
        { id: 'home', children: 'Home', href: '/' },
        {
            id: 'settings',
            children: 'Settings',
            href: '/settings',
            subItems: [
                {
                    id: 'profile',
                    children: 'Profile',
                    href: '/settings/profile'
                },
                {
                    id: 'account',
                    children: 'Account',
                    href: '/settings/account'
                }
            ]
        }
    ]}
    renderLink={item => (
        <a href={item.href} target="_blank" rel="noopener noreferrer">
            {item.children}
        </a>
    )}
/>
```

### Custom Footer Vertical Nav

Drop down to the composed primitives when the nav needs extra chrome (header, footer, version badge). Padding lives on the list, so a bordered footer extends edge-to-edge across the full nav width.

```tsx
<VerticalNavRoot className="flex flex-col">
    <VerticalNavList className="min-h-0 flex-1">
        <VerticalNavListItem>
            <VerticalNavItem href="/data-center">Data Center</VerticalNavItem>
        </VerticalNavListItem>
        <VerticalNavListItem>
            <VerticalNavItem href="/omniverse" active>
                Omniverse
            </VerticalNavItem>
        </VerticalNavListItem>
        <VerticalNavListItem>
            <VerticalNavItem href="/settings">Settings</VerticalNavItem>
        </VerticalNavListItem>
    </VerticalNavList>
    <div className="shrink-0 border-t-1 border-t-base px-4 py-3 text-center text-label-regular-sm text-secondary">
        Version 1.2.3
    </div>
</VerticalNavRoot>
```

### Composed

```tsx
<VerticalNavRoot>
    <VerticalNavList>
        <VerticalNavListItem>
            <VerticalNavItem href="/" active>
                Home
            </VerticalNavItem>
        </VerticalNavListItem>
        <VerticalNavListItem>
            <VerticalNavCollapsibleSection defaultOpen>
                <VerticalNavItem asChild slotEnd={<Icon />}>
                    <VerticalNavCollapsibleTrigger>Settings</VerticalNavCollapsibleTrigger>
                </VerticalNavItem>
                <VerticalNavCollapsibleContent>
                    <VerticalNavSubList>
                        <VerticalNavSubListItem>
                            <VerticalNavItem kind="secondary" href="/settings/profile">
                                Profile
                            </VerticalNavItem>
                        </VerticalNavSubListItem>
                        <VerticalNavSubListItem>
                            <VerticalNavItem kind="secondary" href="/settings/account">
                                Account
                            </VerticalNavItem>
                        </VerticalNavSubListItem>
                    </VerticalNavSubList>
                </VerticalNavCollapsibleContent>
            </VerticalNavCollapsibleSection>
        </VerticalNavListItem>
    </VerticalNavList>
</VerticalNavRoot>
```

## Props

| Prop         | Type                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Default | Description                                                                                                                                                                                                 |
| ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **items** \* | `{ defaultOpen: boolean; subItems: { asChild: boolean; id: string; children: ReactNode; slotIcon: ReactNode; href: string; active: boolean; disabled: boolean }[]; open: boolean; onOpenChange: (open: boolean) => void; asChild: boolean; id: string; children: ReactNode; slotIcon: ReactNode; href: string; active: boolean; disabled: boolean }[]`                                                                                                                                                      | -       | Array of navigation items to render in the vertical nav                                                                                                                                                     |
| asChild      | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | -       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                               |
| renderLink   | `(item: ({ defaultOpen: boolean; subItems: { asChild: boolean; id: string; children: ReactNode; slotIcon: ReactNode; href: string; active: boolean; disabled: boolean }[]; open: boolean; onOpenChange: (open: boolean) => void; asChild: boolean; id: string; children: ReactNode; slotIcon: ReactNode; href: string; active: boolean; disabled: boolean }) \| { asChild: boolean; id: string; children: ReactNode; slotIcon: ReactNode; href: string; active: boolean; disabled: boolean }) => ReactNode` | -       | Custom renderer for navigation links. Used for both primary and secondary items. When provided, the result is wrapped in VerticalNavItem with asChild, so the consumer's element gets all styling/behavior. |

`* = required prop`
