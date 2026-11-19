---
name: kaizen-ui
description: Kaizen UI (KUI) component library and design-pattern advisor for NVIDIA applications. Use whenever someone describes a UI to build, modify, or improve — even without explicit mention of KUI, "design pattern," or "best practice." Covers forms, filters, search, sort, cards, settings, activity feeds, navigation, data visualization, dashboards, loading states, empty states, feedback, iconography, element visibility, and page layout decisions. Also use when modifying existing UI (adding form fields, page sections, nav items, columns); before changes, evaluate whether the current structure still fits and propose restructuring if needed.
metadata:
    # what version of the package these skills are based on
    version: '1.0.0'
# For tanstack-intent
type: core
library: kaizen-ui
library_version: '1.0.0'
---
<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Kaizen UI (KUI)

KUI has three parts, used together:

- **React components** provide the semantic UI: `<Button>`, `<Card>`, `<FormField>`, `<Stack>`, `<Table>`, `<AppBar>`, etc.
- **Layout utilities** — Tailwind's core utilities (`px-4`, `max-w-7xl`, `flex`) handle the space between components.
- **Design tokens** — theme-aware values for colors, borders, and surfaces. Available via Tailwind utilities or CSS variables. See details in [DESIGN_TOKENS.md](./references/guides/DESIGN_TOKENS.md)

| Example Token  | Tailwind utility (only when Tailwind is configured) | CSS variable (for raw CSS or non-Tailwind contexts) |
| -------------- | --------------------------------------------------- | --------------------------------------------------- |
| Sunken surface | `bg-surface-sunken`                                 | `--background-color-surface-sunken`                 |
| Primary text   | `text-primary`                                      | `--color-text-primary`                              |
| Base border    | `border-base`                                       | `--border-color-base`                               |

## General Guidelines

- Use KUI components over divs/custom elements.
- KUI provides semantic CSS variables and Tailwind utilities for the design system — use these instead of standard Tailwind colors for brand consistency and automatic light/dark theme support.
- No emojis.

## References

- [ICONS.md](./references/guides/ICONS.md) describes available icons and how to use them.
- [CONFIGURATION.md](./references/guides/CONFIGURATION.md) describes how to configure KUI for your project - this is useful to reference when running into issues.

- [Accordion](references/components/Accordion.md)
    - A multi-section expand/collapse component for organizing content into collapsible panels. Supports single-open and multi-open modes.
- [Anchor](references/components/Anchor.md)
    - A styled anchor element that provides consistent link styling across applications.
    - sometimes known as Link
- [AnimatedChevron](references/components/AnimatedChevron.md)
    - An internal utility component. Renders a chevron icon that indicates an open or closed state, which is used in accordion- and menu-related components. Reads `data-state` attribute to determine the state.
- [AppBar](references/components/AppBar.md)
    - The top-level masthead for the application. Can contain the NVIDIA logo(or custom branding), navigation, and global actions (user menu, settings). Every application has exactly one AppBar.
    - sometimes known as Masthead, Navbar, Navigation Bar
- [Avatar](references/components/Avatar.md)
    - Use an avatar whenever it's helpful to quickly identify a user. Prioritize images over text to improve clarity. Provide the user's initials as a fallback when no image is available.
- [Badge](references/components/Badge.md)
    - A small color-coded label for indicating status, category, or metadata at a glance. Badges are non-interactive - they display information, not trigger actions.
- [Banner](references/components/Banner.md)
    - Banners communicate important messages and actions that draw the user's attention. Banners should be embedded in the page layout and span the width of its container. Use sparingly.
- [Block](references/components/Block.md)
    - A primitive div component. This component provides a convenient way to create elements with consistent spacing and overflow handling.
- [Breadcrumbs](references/components/Breadcrumbs.md)
    - A navigation path showing the user's location within the apps hierarchy, providing a trail back to parent pages and helping users orient themselves in deep navigation structures. Use concise labels. Do not use `Breadcrumbs` for root or first-level pages - there's nothing to navigate back to.
- [Button](references/components/Button.md)
    - A clickable element that triggers an action. Use specific verb + noun labels instead of vague text.
- [ButtonGroup](references/components/ButtonGroup.md)
    - Use a Button Group to offer a set of choices that are related. It's recommended to show no more than 4 actions. Also consider a Segmented Control when actions are mutually exclusive and must have one active at all times.
- [Card](references/components/Card.md)
    - A card contains content and actions about a single subject. Cards carry identity: they describe "a thing" with its own content, actions, and optional media. By default cards are fluid and will grow to fit their container.
- [Checkbox](references/components/Checkbox.md)
    - Use checkboxes to allow users to select multiple options from a list or to mark a single item as selected
- [CodeSnippet](references/components/CodeSnippet.md)
    - A code snippet component with syntax highlighting, copy functionality, and optional collapse behaviour. Supports both inline code spans and block-level code blocks.
- [Collapsible](references/components/Collapsible.md)
    - A utility component that allows you to collapse and expand content.
- [Combobox](references/components/Combobox.md)
    - A Combobox combines a text input and listbox to let users filter and pick from declarative `items`.
- [DatePicker](references/components/DatePicker.md)
    - A complete date picker component with input field(s) and calendar popup. Supports both single date selection and date range selection modes. The component handles manual date entry through input fields and provides a calendar interface for visual date selection.
- [Divider](references/components/Divider.md)
    - A visual separator that creates a clear distinction between content sections
- [Dropdown](references/components/Dropdown.md)
    - Use Dropdowns to allow users to select one or multiple options, navigate through links, or take actions. Dropdowns are often used for navigation, filters, or contextual actions.
- [Flex](references/components/Flex.md)
    - A primitive flex component.
- [FormField](references/components/FormField.md)
    - A component for displaying a form field with label, description, helper text, and validation messages. FormField automatically adapts to different container widths. For horizontal layouts (`labelPosition="left"`), it will switch to vertical layout on small screens.
    - sometimes known as Form Group, Fieldset, Form Input
- [Grid](references/components/Grid.md)
    - A primitive grid component.
- [Group](references/components/Group.md)
    - Group is a utility component that joins items together visually.
- [Hero](references/components/Hero.md)
    - A Hero is a large page banner that combines important messaging and calls to action with large imagery. Has responsive padding based on the component width.
- [HorizontalNav](references/components/HorizontalNav.md)
    - The HorizontalNav component is a customizable navigation bar designed to display a list of links in a horizontal layout. It allows users to navigate between different sections of an application or website.
- [Inline](references/components/Inline.md)
    - A primitive span component.
- [InputShell](references/components/InputShell.md)
    - Internal utility component. Use this to wrap any element that needs to be styled as an input.
- [Label](references/components/Label.md)
    - A label component for forms and inputs. If you're using this component for a form field, just use the FormField component instead. This component is mostly intended for internal use.
- [List](references/components/List.md)
    - A flexible list component that supports unordered, ordered, and icon lists.
- [Menu](references/components/Menu.md)
    - A static menu component that displays a list of items. Note: if you're looking for a menu that can be used as a dropdown, see the `Dropdown` component. This is the menu component itself and does not have behaviour to hide/show itself.
- [Modal](references/components/Modal.md)
    - A modal dialog that can be triggered by a button or other element. Uses the native `<dialog>` element for progressive enhancement, providing built-in backdrop styling, focus management, and support for `form method="dialog"` for native close behavior.
- [Notification](references/components/Notification.md)
    - A notification component that displays important messages and alerts to users. It provides a flexible layout with support for icons, headings, subheadings, close buttons and footers. The component uses a grid-based layout through NotificationContent to organize its elements: - Icon on the left (optional) - Header content in the middle containing heading and subheading - Close button on the right (optional) - Footer below (optional)
- [PageHeader](references/components/PageHeader.md)
    - The page header describes the current page and displays a title, breadcrumbs, and page-level actions
- [Pagination](references/components/Pagination.md)
    - A pagination component that provides different styles of navigation for paginated content.
- [Panel](references/components/Panel.md)
    - A panel is a simple container used to wrap content and actions for a given component in a card-like format with a background, border, and padding.
- [Popover](references/components/Popover.md)
    - Displays rich content in a portal, triggered by a button click. Use a popover to display specific information, options, or actions related to an element on the page. Built on the native Popover API and CSS Anchor Positioning for progressive enhancement. The trigger uses `popovertarget` so toggling works without JavaScript. Positioning is handled entirely in CSS via `anchor-name` / `position-anchor`.
- [ProgressBar](references/components/ProgressBar.md)
    - Use a progress bar to visually communicate processes that involve long wait times, such as downloading, uploading, submitting, or loading data.
- [RadioGroup](references/components/RadioGroup.md)
    - A set of radio buttons where no more than one of the options can be selected at a time. **Progressive Enhancement:** Uses native `<input type="radio">` elements which provide built-in keyboard navigation (arrow keys), mutual exclusion, and form submission without JavaScript.
- [RangeSlider](references/components/RangeSlider.md)
    - A range slider component allows users to select a range of values (two thumbs). Range sliders reflect a range of values along a bar, from which users may select a range between two values. They are ideal for filtering by price ranges, selecting time windows, or any scenario requiring a min/max selection.
    - sometimes known as Range Input, Input Range
- [SegmentedControl](references/components/SegmentedControl.md)
    - A segmented control allows users to select one option in a set of related options that takes effect on selection. This component is built as a radio group, and uses `name` to group the radio buttons together.
- [Select](references/components/Select.md)
    - A Select is a dropdown that allows the user to select a value from a list of options.
- [SidePanel](references/components/SidePanel.md)
    - A dialog pinned to the left or right side of the screen that can be triggered by a button or other element. Uses the native `<dialog>` element for progressive enhancement, providing built-in backdrop styling, focus management, and support for `form method="dialog"` for native close behavior. Use a side panel when you need to show supplementary content or actions while maintaining the context of the main view. Side panels are ideal for tasks that require user input or displaying additional information without navigating away from the current page. Common use cases include: - Displaying details about a selected item - Forms that need context from the main view - Configuration panels - Multi-step workflows - Preview panels Consider a Modal instead when: - The task requires full user attention and blocking interaction with the main content - The content needs to be center-focused - The interaction is brief and doesn't require context from the main view
- [Skeleton](references/components/Skeleton.md)
    - A skeleton component is a placeholder that mimics the layout of content while it loads, giving users a sense of the structure and reducing perceived wait time. Always try and make the skeleton components as simular in size and shape to the dynamic content as you can
- [Slider](references/components/Slider.md)
    - A slider component allows users to select a single value from a range of values. Sliders reflect a range of values along a bar, from which users may select a single value. They are ideal for adjusting settings such as volume, brightness, or applying image filters.
    - sometimes known as Range Input, Input Range
- [Spinner](references/components/Spinner.md)
    - Use a spinner when the user is waiting on an operation including loading, downloading, uploading, processing, etc. that's indeterminate. Spinners are displayed until content appears or a process is complete. This component should be used when the expected duration is more than a second and the completion time is short (up to 5 seconds). A message can be used with a spinner to improve understanding.
- [Stack](references/components/Stack.md)
    - A container component for laying out content in a vertical or horizontal stack. This is a wrapper around the {@link Flex} component.
- [StatusIndicator](references/components/StatusIndicator.md)
    - A status indicator is a simple circle that is ephemeral and used to signal new changes or, occasionally, status.
- [StatusMessage](references/components/StatusMessage.md)
    - Use a status message when there is no data or content available. This helps inform users of the current status and suggests next steps when appropriate.
- [Stepper](references/components/Stepper.md)
    - A stepper (progress tracker) that visually represents a user's progress through a series of discrete steps.
- [Switch](references/components/Switch.md)
    - A switch allows users to quickly switch between two states. Use a switch to allow users to turn something on or off instantly. Switches are only used with binary actions with a default setting set. It's commonly used for adjusting settings and preferences.
    - sometimes known as Toggle, ToggleSwitch
- [Table](references/components/Table.md)
    - A table organizes sets of data into columns and rows.
- [Tabs](references/components/Tabs.md)
    - A high-level tabs component that provides a simple way to create tabbed interfaces. Wraps the lower-level composed tab components for ease of use.
- [Tag](references/components/Tag.md)
    - Tags are used to categorize, label, or group items using text and optional icons, providing users with quick, contextual information.
- [Text](references/components/Text.md)
    - A primitive component for displaying text.
- [TextArea](references/components/TextArea.md)
    - A textarea with additional capabilities such as icons and status.
- [TextInput](references/components/TextInput.md)
    - This component is a composition of the `InputShell` and `input` components. The `ref` and HTML attributes are applied to the inner `input` component.
- [ThemeProvider](references/components/ThemeProvider.md)
    - A comprehensive theme provider that manages both density (spacing) and theme (light/dark) themes for all child components. This component: - Sets density variants that control spacing throughout child components - Manages color themes with support for system preference detection - Controls motion and animations with support for accessibility preferences - Applies theme classes to the provider element for scoped theming (only when explicitly provided or inherited) - Provides theme context to all descendant components via React Context - Handles system theme changes and updates the UI accordingly - Supports nesting with automatic inheritance from parent providers When nested, child providers inherit density, theme, and defer settings from their parent unless explicitly overridden. If no theme is provided and there's no parent context, no theme class is applied, allowing manually set theme classes to take precedence. This allows for granular theme control in different sections of the application while maintaining consistent defaults and not overriding external theme solutions.
- [Toast](references/components/Toast.md)
    - Use a toast to display small messages without disrupting a user's experience. Toasts are commonly used to provide non-critical, contextual feedback following a user's action or process. They're low-emphasis and appear temporarily as an overlay.
- [Tooltip](references/components/Tooltip.md)
    - A tooltip displays additional information when users hover or focus on an element. To remove the delay on hover when switching between different tooltips, it is highly recommended to wrap your application in a `TooltipProvider` component. Built on the native Popover API and CSS Anchor Positioning for progressive enhancement. Hover and focus interactions are managed with lightweight JavaScript; uncontrolled tooltips also render a no-JS click target during SSR.
- [TreeNav](references/components/TreeNav.md)
    - A tree navigation component that lets users move through links at different levels, like a directory structure. Uses native `<details>`/`<summary>` elements for zero-JS expand/collapse behavior. Branches containing an active descendant are automatically expanded unless they use controlled state (`open` prop) or have an explicit `defaultOpen`.
- [Upload](references/components/Upload.md)
    - Upload is a wrapper around `input type="file"` that also renders previews for uploaded files and provides basic file management. Note that file validation and actual upload is not handled by this component beyond what the input element provides.
- [VerticalNav](references/components/VerticalNav.md)
    - The VerticalNav component provides a vertical navigation menu that can handle both single links and nested link groups using native HTML `<details>`/`<summary>`.
