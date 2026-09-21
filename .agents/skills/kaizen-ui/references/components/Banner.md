<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Banner

Banners communicate important messages and actions that draw the user's attention. Banners should
be embedded in the page layout and span the width of its container. Use sparingly.

## Examples

### Inline Banner

```tsx
<Flex className="w-full" gap="1" direction="col">
    <Banner status="info">Lorem ipsum dolor sit</Banner>
    <Banner status="success">Lorem ipsum dolor sit</Banner>
    <Banner status="warning">Lorem ipsum dolor sit</Banner>
</Flex>
```

### Global Banner

```tsx
<Banner onClose={hideBanner} status="warning" kind="global">
    Your account password is expiring soon
</Banner>
```

### Header Banner With Actions

```tsx
<Banner
    kind="header"
    onClose={hideBanner}
    slotSubheading="Our system will be unavailable on Saturday from 2-4 AM"
    slotActions={
        <>
            <Button kind="secondary" size="tiny">
                Support
            </Button>
            <Button kind="secondary" size="tiny">
                Add to Calendar
            </Button>
        </>
    }
>
    Service maintenance scheduled
</Banner>
```

### Composed

```tsx
<BannerRoot kind="global" status="warning">
    <BannerLayout>
        <BannerContent>
            <BannerIcon>
                <Warning />
            </BannerIcon>
            <BannerHeader>
                <BannerHeading>Service maintenance scheduled</BannerHeading>
            </BannerHeader>
        </BannerContent>
        <BannerActionsSection>
            <Button kind="secondary" size="tiny">
                Support
            </Button>
        </BannerActionsSection>
        <BannerCloseButtonSection>
            <Button aria-label="Close" kind="tertiary" size="tiny">
                <Close />
            </Button>
        </BannerCloseButtonSection>
    </BannerLayout>
</BannerRoot>
```

### Composed Header Example

```tsx
<BannerRoot kind="header" status="success">
    <BannerLayout>
        <BannerContent>
            <BannerIcon>
                <SuccessIcon />
            </BannerIcon>
            <BannerHeader>
                <BannerHeading>Maintenance complete</BannerHeading>
                <BannerSubheading>Our system is back online</BannerSubheading>
            </BannerHeader>
        </BannerContent>
        <BannerActionsSection>
            <Button kind="secondary" size="tiny">
                Changelog
            </Button>
        </BannerActionsSection>
        <BannerCloseButtonSection>
            <Button aria-label="Close" kind="tertiary" size="tiny">
                <Close />
            </Button>
        </BannerCloseButtonSection>
    </BannerLayout>
</BannerRoot>
```

## Props

| Prop                  | Type                                                                                                                           | Default                                                                                                                                                                                                                                                                                  | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **kind** \*           | `"header" \| "inline" \| "global"`                                                                                             | `"inline"`                                                                                                                                                                                                                                                                               | The kind of banner to render. Choose based on the scope of the message. - `inline`: Contextual alerts to be used in a specific content area. Should be placed in the content flow. - `header`: A banner with a bolded heading and optional subheading. Should be rendered below the AppBar or at the top of a page section. - `global`: A banner that should be used for site-wide messaging. Should be placed at the top of the entire app, above the AppBar. |
| **slotSubheading** \* | `ReactNode \| never`                                                                                                           | -                                                                                                                                                                                                                                                                                        | "header" kind banners render their `children` as the heading - `slotSubheading` is used to render the (smaller) subheading text.                                                                                                                                                                                                                                                                                                                               |
| actionsPosition       | `"right" \| "bottom"`                                                                                                          | -                                                                                                                                                                                                                                                                                        | The position of the actions to render in the banner. By default, the actions will be rendered on the right until the banner is too small. Then they will move to the bottom. You probably don't want to use this prop unless you have a specific reason to do so.                                                                                                                                                                                              |
| children              | `ReactNode`                                                                                                                    | -                                                                                                                                                                                                                                                                                        | The heading text of the banner.                                                                                                                                                                                                                                                                                                                                                                                                                                |
| onClose               | `() => void`                                                                                                                   | -                                                                                                                                                                                                                                                                                        | Handler for dismissing the banner. When provided, renders a close button. Omit for blocking banners (e.g. active outages, compliance notices) that must remain visible until resolved.                                                                                                                                                                                                                                                                         |
| ref                   | `((instance: HTMLDivElement) => void \| (() => void \| { [UNDEFINED_VOID_ONLY]: never; })) \| React.RefObject<HTMLDivElement>` | -                                                                                                                                                                                                                                                                                        | Allows getting a ref to the component instance. Once the component unmounts, React will set `ref.current` to `null` (or call the ref with `null` if you passed a callback ref).                                                                                                                                                                                                                                                                                |
| slotActions           | `ReactNode`                                                                                                                    | -                                                                                                                                                                                                                                                                                        | The actions to render in the banner.                                                                                                                                                                                                                                                                                                                                                                                                                           |
| slotIcon              | `ReactNode`                                                                                                                    | `status === "info" ? ( <Icon name="info-circle" variant="fill" /> ) : status === "warning" ? ( <Icon name="warning" variant="fill" /> ) : status === "error" ? ( <Icon name="error" variant="fill" /> ) : status === "success" ? ( <Icon name="check-circle" variant="fill" /> ) : null` | Slot for the icon to render in the banner. By default, the icon rendered is based on the status prop of the banner. To render no icon, pass `null` to this prop.                                                                                                                                                                                                                                                                                               |
| status                | `"error" \| "warning" \| "success" \| "info"`                                                                                  | `"info"`                                                                                                                                                                                                                                                                                 | The status of the banner - based on message semantics. - `info`: Provides neutral information (blue) - `warning`: Alerts users to a potentially unwanted outcome or upcoming issues (yellow) - `error`: Indicates a problem that needs attention, outages, or failures (red) - `success`: Confirms a successful action or operation, or resolved issues (green)                                                                                                |

`* = required prop`
