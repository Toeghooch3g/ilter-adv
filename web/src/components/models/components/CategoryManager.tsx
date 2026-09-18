import { useState } from 'react'
import { toast } from 'sonner'
import { Button } from '../../ui/button'
import { Plus, X } from '../../ui/icons'

interface CategoryManagerProps {
  categories: string[]
  onAdd: (name: string) => Promise<unknown>
  onRemove: (name: string) => Promise<unknown>
}

const DEFAULT_CATEGORIES = new Set(['free', 'economy', 'standard', 'premium', 'embedding'])

/**
 * CategoryManager is the add/remove UI for user-defined model categories,
 * shown in the models view header next to the filter pills. Default
 * categories can't be removed (backend enforces it too), and removing a
 * category still in use is rejected with a 409 that surfaces as a toast.
 */
export function CategoryManager({ categories, onAdd, onRemove }: CategoryManagerProps) {
  const [newCategory, setNewCategory] = useState('')

  const handleAdd = async () => {
    const name = newCategory.trim().toLowerCase()
    if (!name) return
    try {
      await onAdd(name)
      setNewCategory('')
      toast.success('Category added', { description: `"${name}" is now available for models.` })
    } catch {
      toast.error('Add failed', { description: 'Could not add category. It may already exist or the name is invalid.' })
    }
  }

  const handleRemove = async (name: string) => {
    try {
      await onRemove(name)
      toast.success('Category removed', { description: `"${name}" was deleted.` })
    } catch {
      toast.error('Remove failed', {
        description: `"${name}" is still in use by at least one model, or could not be removed.`,
      })
    }
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="flex items-center gap-1">
        <input
          type="text"
          value={newCategory}
          onChange={(e) => setNewCategory(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void handleAdd()
          }}
          placeholder="new category"
          className="w-32 rounded-lg border border-surface-300 bg-white px-2 py-1 text-xs text-surface-700 placeholder-surface-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
        />
        <Button variant="outline" size="sm" onClick={() => void handleAdd()}>
          <Plus size={12} />
        </Button>
      </div>

      {categories.map((c) =>
        DEFAULT_CATEGORIES.has(c) ? null : (
          <button
            key={c}
            type="button"
            onClick={() => void handleRemove(c)}
            className="inline-flex items-center gap-1 rounded-full bg-surface-100 px-2 py-0.5 text-xs font-medium text-surface-600 hover:bg-error/10 hover:text-error"
            title={`Remove category "${c}"`}
          >
            {c}
            <X size={12} />
          </button>
        ),
      )}
    </div>
  )
}
