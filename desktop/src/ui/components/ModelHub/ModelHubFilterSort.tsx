// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo, useCallback } from 'react'
import { Button, Flex, Text } from '@nvidia/foundations-react-core'
import { ArrowDownward, ArrowUpward, FilterList } from '@/ui/components/icons'
import CascadeMenu from '@/ui/components/CascadeMenu/CascadeMenu'
import { SORT_FIELD, SORT_FIELDS, SortState } from '@/ui/types/model-hub'
import { sortFieldLabel } from '@/ui/utils/model-hub-search'
import { CascadeMenuItem } from '@/ui/types/cascade-menu'

type ModelHubFilterSortProps = {
    sort: SortState
    onSort: (sort: SortState) => void
    onMenuOpenChange?: (open: boolean) => void
}

function sortLabel(field: SORT_FIELD, descending: boolean, active: boolean) {
    const iconClass = '-mt-0.5 min-w-[16px]'
    const icon = descending ? (
        <ArrowDownward style={{ fontSize: 16 }} className={iconClass} />
    ) : (
        <ArrowUpward style={{ fontSize: 16 }} className={iconClass} />
    )
    return (
        <Flex align="center" justify="between" wrap="nowrap" gap="2" className="w-full">
            <Text kind={active ? 'body/bold/sm' : 'body/regular/sm'}>{sortFieldLabel(field)}</Text>
            {active && icon}
        </Flex>
    )
}

export const ModelHubFilterSort = ({ sort, onSort, onMenuOpenChange }: ModelHubFilterSortProps) => {
    const handleSort = useCallback(
        (field: SORT_FIELD) => {
            const isThisField = field === sort.sort
            const newAllDirections = { ...sort.allDirections }
            const newSortDescending = isThisField ? !sort.sortDescending : newAllDirections[field]
            newAllDirections[field] = newSortDescending
            onSort({
                sort: field,
                sortDescending: newSortDescending,
                allDirections: newAllDirections
            })
        },
        [onSort, sort]
    )

    const items: CascadeMenuItem[] = useMemo(
        () =>
            SORT_FIELDS.map(field => {
                const isActive = sort.sort === field
                return {
                    id: field,
                    label: sortLabel(
                        field,
                        isActive ? sort.sortDescending : sort.allDirections[field],
                        isActive
                    ),
                    onSelect: () => handleSort(field)
                }
            }),
        [sort, handleSort]
    )

    return (
        <CascadeMenu
            items={items}
            onOpenChange={onMenuOpenChange}
            trigger={
                <Button kind="secondary" size="small" aria-label="Sort models">
                    <FilterList style={{ fontSize: 14 }} />
                </Button>
            }
        />
    )
}
