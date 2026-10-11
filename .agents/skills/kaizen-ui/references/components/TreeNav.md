<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# TreeNav

A tree navigation component that lets users move through links at different
levels, like a directory structure. Uses native `<details>`/`<summary>`
elements for zero-JS expand/collapse behavior.

Branches containing an active descendant are automatically expanded unless
they use controlled state (`open` prop) or have an explicit `defaultOpen`.

## Examples

### Basic Tree Nav

Use for hierarchical navigation structures like file trees or nested categories.

```tsx
<TreeNav
    items={[
        {
            id: 'src',
            children: 'src',
            defaultOpen: true,
            items: [
                { id: 'index', children: 'index.ts', href: '#index' },
                { id: 'utils', children: 'utils.ts', href: '#utils' }
            ]
        },
        { id: 'readme', children: 'README.md', href: '#readme' }
    ]}
/>
```

### With Icons Tree Nav

Pass `slotIcon` on any item to add an icon to a branch trigger or override the default document icon on a leaf.

```tsx
<TreeNav
    items={[
        {
            id: 'inbox',
            children: 'Inbox',
            slotIcon: <Bell />,
            defaultOpen: true,
            items: [
                {
                    id: 'drafts',
                    children: 'Drafts',
                    slotIcon: <CopyDoc />,
                    href: '#drafts'
                },
                {
                    id: 'scheduled',
                    children: 'Scheduled',
                    slotIcon: <Calendar />,
                    href: '#scheduled'
                }
            ]
        },
        {
            id: 'trash',
            children: 'Trash',
            slotIcon: <Trash />,
            href: '#trash'
        }
    ]}
/>
```

### With Default Open Tree Nav

Set `defaultOpen` on a branch to render it expanded on initial mount while still allowing the user to collapse it.

```tsx
<TreeNav
    items={[
        {
            id: 'src',
            children: 'src',
            defaultOpen: true,
            items: [
                { id: 'index', children: 'index.ts', href: '#index' },
                {
                    id: 'components',
                    children: 'components',
                    items: [{ id: 'button', children: 'Button.tsx', href: '#button' }]
                }
            ]
        }
    ]}
/>
```

### With Active Item Tree Nav

Use when the current location should be highlighted. Branches with active descendants auto-expand.

```tsx
<TreeNav
    items={[
        {
            id: 'docs',
            children: 'docs',
            items: [
                {
                    id: 'getting-started',
                    children: 'Getting Started',
                    href: '#getting-started',
                    active: true
                },
                { id: 'api', children: 'API Reference', href: '#api' }
            ]
        },
        { id: 'changelog', children: 'Changelog', href: '#changelog' }
    ]}
/>
```

### With Disabled Items Tree Nav

Set `disabled` on a leaf to make it non-interactive, or on a branch to disable the entire subtree — descendants inherit the disabled state.

```tsx
<TreeNav
    items={[
        {
            id: 'archive',
            children: 'Archive (read-only)',
            disabled: true,
            defaultOpen: true,
            items: [{ id: 'old', children: 'old-file.ts', href: '#old' }]
        },
        {
            id: 'src',
            children: 'src',
            defaultOpen: true,
            items: [
                { id: 'app', children: 'App.tsx', href: '#app' },
                {
                    id: 'deprecated',
                    children: 'deprecated.ts',
                    href: '#deprecated',
                    disabled: true
                }
            ]
        }
    ]}
/>
```

### With Loading State Tree Nav

Set `loading` on an item to replace its icon with a spinner — typically combined with `onOpenChange` or `onSelect` to drive async data fetches.

```tsx
<TreeNav
    items={[
        {
            id: 'api',
            children: 'api',
            defaultOpen: true,
            loading: true,
            items: [
                {
                    id: 'routes',
                    children: 'Fetching routes.ts...',
                    href: '#routes',
                    loading: true
                }
            ]
        }
    ]}
/>
```

### With Event Handlers Tree Nav

Use `onSelect` on leaves to react to clicks (analytics, tracking) and `onOpenChange` on branches to react to expand/collapse without taking control of the open state.

```tsx
<TreeNav
    items={[
        {
            id: 'src',
            children: 'src',
            defaultOpen: true,
            onOpenChange: handleOpenChange,
            items: [
                {
                    id: 'app',
                    children: 'App.tsx',
                    href: '#app',
                    onSelect: handleSelect
                }
            ]
        }
    ]}
/>
```

### Controlled Tree Nav

Pass both `open` and `onOpenChange` to drive branch expansion from external state (Redux, URL, parent component, etc.) instead of letting the component manage it.

```tsx
<TreeNav
    items={[
        {
            id: 'src',
            children: 'src',
            open: true,
            onOpenChange: handleOpenChange,
            items: [{ id: 'app', children: 'App.tsx', href: '#app' }]
        },
        {
            id: 'lib',
            children: 'lib',
            open: false,
            onOpenChange: handleOpenChange,
            items: [{ id: 'utils', children: 'utils.ts', href: '#utils' }]
        }
    ]}
/>
```

### Non Collapsible Branch Tree Nav

Use when a branch should always remain expanded and cannot be collapsed by the user — useful for top-level categories that anchor the navigation.

```tsx
<TreeNav
    items={[
        {
            id: 'root',
            children: 'Project Root',
            collapsible: false,
            items: [
                { id: 'package', children: 'package.json', href: '#package' },
                { id: 'tsconfig', children: 'tsconfig.json', href: '#tsconfig' }
            ]
        }
    ]}
/>
```

### With Render Link Tree Nav

Use `renderLink` to swap the built-in `<a>` for a framework router link (Next.js `<Link>`, React Router `<NavLink>`, TanStack Router, etc.).

```tsx
<TreeNav
    items={[
        {
            id: 'pages',
            children: 'pages',
            defaultOpen: true,
            items: [
                { id: 'home', children: 'Home', href: '/home' },
                { id: 'about', children: 'About', href: '/about' }
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

### Composed

```tsx
<TreeNavRoot>
    <TreeNavList>
        <TreeNavBranch defaultOpen>
            <TreeNavBranchTrigger>src</TreeNavBranchTrigger>
            <TreeNavList>
                <TreeNavLeaf href="#index">index.ts</TreeNavLeaf>
                <TreeNavBranch defaultOpen>
                    <TreeNavBranchTrigger>components</TreeNavBranchTrigger>
                    <TreeNavList>
                        <TreeNavLeaf href="#button" active>
                            Button.tsx
                        </TreeNavLeaf>
                    </TreeNavList>
                </TreeNavBranch>
            </TreeNavList>
        </TreeNavBranch>
        <TreeNavLeaf href="#readme">README.md</TreeNavLeaf>
    </TreeNavList>
</TreeNavRoot>
```

## Props

| Prop         | Type                                                                                                                                                                                                                                                                                                                                                                    | Default | Description |
| ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | ----------- |
| **items** \* | `{ id: string; children: ReactNode; slotIcon: ReactNode; href: string; active: boolean; disabled: boolean; asChild: boolean; defaultOpen: boolean; open: boolean; onOpenChange: (open: boolean) => void; collapsible: boolean; loading: boolean; onSelect: (event: React.MouseEvent<HTMLElement, MouseEvent>) => void; items: TreeNavBaseItem[] }[]`                    | -       |             |
| renderLink   | `(item: { id: string; children: ReactNode; slotIcon: ReactNode; href: string; active: boolean; disabled: boolean; asChild: boolean; defaultOpen: boolean; open: boolean; onOpenChange: (open: boolean) => void; collapsible: boolean; loading: boolean; onSelect: (event: React.MouseEvent<HTMLElement, MouseEvent>) => void; items: TreeNavBaseItem[] }) => ReactNode` | -       |             |

`* = required prop`
