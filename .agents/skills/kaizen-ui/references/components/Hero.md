<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Hero

A Hero is a large page banner that combines important messaging and calls to action with large imagery.

Has responsive padding based on the component width.

## Notes

- By default content is left aligned which is preferred for most use cases. To center or right
  align content, you can use `text-center` or `text-right` on the Hero component.

## Examples

### Basic Hero

Use for prominent page banners that communicate key messaging without media.

```tsx
<Hero
    slotHeading="Welcome to the Platform"
    slotBody="Get started by exploring available models, tools, and resources."
/>
```

### With Subheading Hero

Use when a secondary label above the heading provides additional context like a category or publisher name.

```tsx
<Hero
    slotSubheading="Getting Started"
    slotHeading="Build with NVIDIA"
    slotBody="Access pre-trained models, SDKs, and APIs to accelerate your AI projects."
/>
```

### With Actions Hero

Use when the hero needs prominent call-to-action buttons below the body text.

```tsx
<Hero
    slotSubheading="New Release"
    slotHeading="Model Hub"
    slotBody="Discover, customize, and deploy foundation models."
    slotActions={
        <>
            <Button kind="secondary">Learn More</Button>
            <Button color="brand">Get Started</Button>
        </>
    }
/>
```

### With Media Hero

Pass `slotMedia` for a hero with a background image, then set `mediaTheme` to `dark` (default) or `light` so the heading and body text contrast against it.

```tsx
<Hero
    mediaTheme="dark"
    slotHeading="Deep Learning Institute"
    slotBody="Hands-on training for developers, data scientists, and researchers."
    slotMedia={<Media />}
/>
```

### Custom Heading Hero

Use when the heading needs richer content than a string. slotHeading accepts any ReactNode, so you can compose layout primitives, badges, or icons inline.

```tsx
<Hero
    slotSubheading="Kaizen Design System"
    slotHeading={
        <Flex direction="col" gap="density-md">
            <span>Kaizen UI Foundations</span>
            <Flex gap="density-md" wrap="wrap">
                <Badge kind="solid">Accessible</Badge>
                <Badge kind="solid">Responsive</Badge>
                <Badge kind="solid">CSS Only</Badge>
            </Flex>
        </Flex>
    }
    slotBody="A CSS-only implementation of the NVIDIA Design Language."
/>
```

### Aligned Hero

Use the text-center or text-right utility class on the Hero when the design calls for non-default alignment. Left alignment is preferred for most cases.

```tsx
<Hero
    className="text-center"
    slotSubheading="Getting Started"
    slotHeading="Centered Hero"
    slotBody="Hero content is left-aligned by default. Apply the text-center or text-right utility class to the Hero to override the alignment."
    slotActions={
        <>
            <Button kind="secondary">Learn More</Button>
            <Button color="brand">Get Started</Button>
        </>
    }
/>
```

### Custom Styling Hero

Use the Hero CSS variables (--padding, --max-width, --text-color) to customize spacing, width, or text color while keeping the rest of the component's styling.

```tsx
<Hero
    style={{ '--padding': '0', '--max-width': '100%' }}
    slotHeading="Edge-to-Edge Hero"
    slotBody="Override the --padding, --max-width, and --text-color CSS variables to fine-tune hero spacing and width without forking the component."
    slotActions={<Button color="brand">Get Started</Button>}
/>
```

### Composed

```tsx
<HeroRoot>
    <HeroContent>
        <HeroSubheading>Subheading</HeroSubheading>
        <HeroHeading>Hero Heading</HeroHeading>
        <HeroBody>Hero body content with a description of the page or feature.</HeroBody>
        <HeroFooter>
            <Button color="brand">Primary Action</Button>
        </HeroFooter>
    </HeroContent>
</HeroRoot>
```

## Props

| Prop               | Type                | Default | Description                                                                                                                                                                                                                                                                                                               |
| ------------------ | ------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **slotBody** \*    | `ReactNode`         | -       | The slot for the body content of the component. Renders below the heading and above the action slots. Has text styling that is consistent across all widths.                                                                                                                                                              |
| **slotHeading** \* | `ReactNode`         | -       | The slot for the heading of the component. The largest and main text of the hero. Renders below the subheading, and above the body and action slots. Has responsive text styles based on the component width.                                                                                                             |
| asChild            | `boolean`           | -       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                             |
| mediaTheme         | `"light" \| "dark"` | -       | Indicates whether the Hero media is visually light or dark. Applies the appropriate NV theme class ("nv-light" or "nv-dark") to optimize text contrast and readability. For example, if the media is a black image you should set `mediaTheme="dark"`. If the media is a white image you should set `mediaTheme="light"`. |
| slotActions        | `ReactNode`         | -       | The slot for the action area of the hero component. Renders below the body at the bottom of the hero.                                                                                                                                                                                                                     |
| slotMedia          | `ReactNode`         | -       | Slot for media content to render as the background of the hero. If `slotMedia` is set you should also set `mediaTheme` to ensure text and components are readable.                                                                                                                                                        |
| slotSubheading     | `ReactNode`         | -       | The slot for the optional subheading of the component. Renders above the heading in smaller text. Has responsive text styles based on the component width.                                                                                                                                                                |

`* = required prop`
