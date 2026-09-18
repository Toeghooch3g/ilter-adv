interface CategorySelectProps {
  categories: readonly string[]
  category: string
  onChange: (category: string) => void
}

/**
 * CategorySelect is the per-card category picker on the models view: setting
 * a model's category happens directly from the card, no configure modal.
 */
export function CategorySelect({ categories, category, onChange }: CategorySelectProps) {
  return (
    <select
      value={category}
      onChange={(e) => onChange(e.target.value)}
      className="rounded-lg border border-surface-300 bg-white px-2 py-1 text-xs font-medium text-surface-700 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
      title="Model category"
    >
      {categories.map((c) => (
        <option key={c} value={c}>
          {c}
        </option>
      ))}
    </select>
  )
}
