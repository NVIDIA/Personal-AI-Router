// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { Dropdown, Flex, Text, type DropdownEntry } from '@nvidia/foundations-react-core'
import { FilterList } from './icons'
import { JobsFilterType } from '@/ui/types/types'

interface JobsFilterProps {
    setFilter: (type: JobsFilterType, checked: boolean) => void
    values: {
        active: { checked: boolean; count: number }
        completed: { checked: boolean; count: number }
        failed: { checked: boolean; count: number }
    }
}

function JobFilterItem({ id, count }: { id: JobsFilterType; count: number }) {
    return (
        <Flex align="center" justify="between" gap="2" className="ml-1 min-w-24">
            <Text kind="body/regular/sm" className="capitalize">
                {id}
            </Text>
            <Text kind="body/regular/sm" className="text-subtle-color">
                {count > 0 ? `(${count})` : ''}
            </Text>
        </Flex>
    )
}

export default function JobsFilter({ setFilter, values }: JobsFilterProps) {
    const dropdownItems: DropdownEntry[] = useMemo(
        () => [
            {
                kind: 'checkbox',
                checked: values.active.checked,
                onCheckedChange: checked => setFilter('active', checked === true),
                children: <JobFilterItem id="active" count={values.active.count} />
            },
            {
                kind: 'checkbox',
                checked: values.completed.checked,
                onCheckedChange: checked => setFilter('completed', checked === true),
                children: <JobFilterItem id="completed" count={values.completed.count} />
            },
            {
                kind: 'checkbox',
                checked: values.failed.checked,
                onCheckedChange: checked => setFilter('failed', checked === true),
                children: <JobFilterItem id="failed" count={values.failed.count} />
            }
        ],
        [setFilter, values]
    )

    return (
        <Dropdown
            items={dropdownItems}
            attributes={{
                DropdownContent: { className: 'no-drag-elements jobs-filter-dropdown-content' }
            }}
        >
            <FilterList style={{ fontSize: 16 }} />
            <Text kind="body/bold/sm">Jobs</Text>
        </Dropdown>
    )
}
