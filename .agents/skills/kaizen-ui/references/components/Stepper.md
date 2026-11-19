<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Stepper

A stepper (progress tracker) that visually represents a user's progress through a series of discrete steps.

## Examples

### Basic Stepper

Use to visually represent progress through a multi-step process like a checkout or wizard flow.

```tsx
<Stepper
    activeStep={1}
    aria-label="Checkout progress"
    items={[
        { slotHeading: 'Cart', slotDescription: 'Review your items' },
        { slotHeading: 'Shipping', slotDescription: 'Enter your address' },
        { slotHeading: 'Payment', slotDescription: 'Complete purchase' }
    ]}
/>
```

### Vertical Stepper

Use when steps have lengthy descriptions or content, or when the layout requires vertical stacking.

```tsx
<Stepper
    layout="vertical"
    activeStep={1}
    aria-label="Setup wizard"
    items={[
        { slotHeading: 'Account', slotDescription: 'Create your account' },
        { slotHeading: 'Profile', slotDescription: 'Set up your profile' },
        { slotHeading: 'Review', slotDescription: 'Confirm your details' }
    ]}
/>
```

### Compact Stepper

Use when space is limited and a minimal dot-based indicator is sufficient.

```tsx
<Stepper
    kind="compact"
    activeStep={1}
    aria-label="Upload progress"
    items={[
        { slotHeading: 'Upload' },
        { slotHeading: 'Process', slotDescription: 'Step 2 of 3' },
        { slotHeading: 'Done' }
    ]}
/>
```

### Completed Stepper

Use when all steps are complete by setting activeStep beyond the last index.

```tsx
<Stepper
    activeStep={3}
    aria-label="Completed flow"
    items={[
        { slotHeading: 'Step One' },
        { slotHeading: 'Step Two' },
        { slotHeading: 'Step Three' }
    ]}
/>
```

### With Error Stepper

Use when a step has failed validation or encountered an error that needs user attention.

```tsx
<Stepper
    activeStep={1}
    aria-label="Deployment progress"
    items={[
        { slotHeading: 'Configure' },
        { slotHeading: 'Validate', status: 'error' },
        { slotHeading: 'Deploy' }
    ]}
/>
```

### With Content Stepper

Use slotContent to render arbitrary content (such as the form fields for the active step) directly under each step. Only renders in the default (non-compact) kind.

```tsx
<Stepper
    activeStep={1}
    aria-label="Onboarding"
    items={[
        {
            slotHeading: 'Account',
            slotDescription: 'Account created.'
        },
        {
            slotHeading: 'Profile',
            slotDescription: 'Tell us about yourself.',
            slotContent: <div>Profile form fields go here</div>
        },
        {
            slotHeading: 'Review',
            slotDescription: 'Confirm your details.'
        }
    ]}
/>
```

### Clickable Nodes Stepper

Use slotStepIndicator to make step nodes interactive (links or buttons) so users can navigate between steps. Completed steps still show the check icon while keeping the indicator clickable underneath.

```tsx
<Stepper
    activeStep={1}
    aria-label="Navigable progress"
    items={[
        {
            slotHeading: 'Cart',
            slotDescription: 'Review your items.',
            slotStepIndicator: <a href="#cart">1</a>
        },
        {
            slotHeading: 'Shipping',
            slotDescription: 'Enter your address.',
            slotStepIndicator: <a href="#shipping">2</a>
        },
        {
            slotHeading: 'Payment',
            slotDescription: 'Complete your purchase.',
            slotStepIndicator: <a href="#payment">3</a>
        }
    ]}
/>
```

### Compact Vertical Stepper

Use to combine the compact dot indicator with vertical stacking — fits narrow side panels or sidebars where horizontal space is limited.

```tsx
<Stepper
    layout="vertical"
    kind="compact"
    activeStep={1}
    aria-label="Upload progress"
    items={[
        { slotHeading: 'Upload', slotDescription: 'Steps 1/3 completed.' },
        { slotHeading: 'Process' },
        { slotHeading: 'Review' }
    ]}
/>
```

### With Item Attributes Stepper

Use the per-item `attributes` prop to attach analytics tags, custom data attributes, or override accessibility props on a specific step.

```tsx
<Stepper
    activeStep={1}
    aria-label="Deployment progress"
    items={[
        {
            slotHeading: 'Build',
            slotDescription: 'Compile artifacts.',
            attributes: {
                StepperItem: {
                    // @ts-expect-error - data attributes are not typed
                    'data-analytics-id': 'build-step'
                }
            }
        },
        {
            slotHeading: 'Test',
            slotDescription: 'Run integration tests.',
            attributes: {
                StepperItem: {
                    // @ts-expect-error - data attributes are not typed
                    'data-analytics-id': 'test-step'
                }
            }
        },
        {
            slotHeading: 'Deploy',
            slotDescription: 'Release to production.',
            attributes: {
                StepperItem: {
                    // @ts-expect-error - data attributes are not typed
                    'data-analytics-id': 'deploy-step'
                }
            }
        }
    ]}
/>
```

### Composed

```tsx
<StepperRoot activeStep={1} layout="horizontal" aria-label="Progress">
    <StepperItem value={0} slotHeading="Step One" />
    <StepperItem value={1} slotHeading="Step Two" slotDescription="Current step" />
    <StepperItem value={2} slotHeading="Step Three" />
</StepperRoot>
```

## Props

| Prop         | Type                                                                                                                              | Default        | Description                                     |
| ------------ | --------------------------------------------------------------------------------------------------------------------------------- | -------------- | ----------------------------------------------- |
| **items** \* | `{ slotHeading: ReactNode; slotDescription: ReactNode; slotContent: ReactNode; slotStepIndicator: ReactNode; status: "error" }[]` | -              | The items to render in the stepper.             |
| activeStep   | `number`                                                                                                                          | `0`            | The 0-based index of the currently active step. |
| kind         | `"default" \| "compact"`                                                                                                          | `"default"`    | The kind variant of the stepper.                |
| layout       | `"horizontal" \| "vertical"`                                                                                                      | `"horizontal"` | The layout direction of the stepper.            |

`* = required prop`
