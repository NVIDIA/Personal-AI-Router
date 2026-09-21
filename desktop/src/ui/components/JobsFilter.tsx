// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { Dropdown, Flex, Text, type DropdownEntry } from '@nvidia/foundations-react-core'
import { FilterList } from './icons'
import { JobsFilterType } from '@/ui/types/types'

interface JobsFilterProps {
    setFilter: (type: JobsFilterType, checked: boolean) => void
    values: Record<JobsFilterType, { checked: boolean; count: number }>
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

// Dropdown order, listed once so a new bucket needs one entry rather than a
// parallel edit in three places.
const FILTER_ORDER: JobsFilterType[] = ['active', 'completed', 'failed', 'cancelled']

export default function JobsFilter({ setFilter, values }: JobsFilterProps) {
    const dropdownItems: DropdownEntry[] = useMemo(
        () =>
            FILTER_ORDER.map(id => ({
                kind: 'checkbox',
                checked: values[id].checked,
                onCheckedChange: checked => setFilter(id, checked === true),
                children: <JobFilterItem id={id} count={values[id].count} />
            })),
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
