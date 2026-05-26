import { Tab } from 'bootstrap';
import clsx from 'clsx';
import tippy from 'tippy.js';
import { ref } from 'tsx-vanilla';

import { REPO_RELEASES_URL } from '../../constants/other';
import { IndividualSimUI } from '../../individual_sim_ui';
import i18n from '../../../i18n/config';
import {
	BulkComboChoice,
	BulkComboDimension,
	BulkComboSimRequest,
	BulkComboSlotItem,
	BulkSettings,
	DistributionMetrics,
	GemStatCap,
	ProgressMetrics,
	RaidSimRequest,
	RaidSimResult,
} from '../../proto/api';
import { GemColor, HandType, ItemRandomSuffix, ItemSlot, ItemSpec, Profession, RangedWeaponType, WeaponType } from '../../proto/common';
import { ItemEffectRandPropPoints, SimDatabase, SimEnchant, SimGem, SimItem } from '../../proto/db';
import { UIEnchant, UIGem, UIItem } from '../../proto/ui';
import { ActionId } from '../../proto_utils/action_id';
import { EquippedItem } from '../../proto_utils/equipped_item';
import { Gear } from '../../proto_utils/gear';
import { getEmptyGemSocketIconUrl, getMetaGemCondition } from '../../proto_utils/gems';
import { canEquipItem, getEligibleItemSlots, isSecondaryItemSlot } from '../../proto_utils/utils';
import { RequestTypes } from '../../sim_signal_manager';
import { TypedEvent } from '../../typed_event';
import { formatDuration, formatToCompactNumber, formatToNumber, getEnumValues, isExternal, runWithConcurrency, sleep } from '../../utils';
import { ItemData } from '../gear_picker/item_list';
import SelectorModal from '../gear_picker/selector_modal';
import { SimTab } from '../sim_tab';
import Toast from '../toast';
import BulkItemPickerGroup from './bulk/bulk_item_picker_group';
import BulkItemSearch from './bulk/bulk_item_search';
import BulkSimResultRenderer from './bulk/bulk_sim_results_renderer';
import GemSelectorModal from './bulk/gem_selector_modal';
import {
	binomialCoefficient,
	BulkSimItemSlot,
	bulkSimItemSlotToSingleItemSlot,
	bulkSimItemSlotToItemSlotPairs,
	getAllPairs,
	getBulkItemSlotFromSlot,
} from './bulk/utils';
import { BulkGearJsonImporter } from './importers';
import { trackEvent } from '../../../tracking/utils';
import { EnumPicker } from '../pickers/enum_picker';
import { translateBulkSlotName, translateWeaponType } from '../../../i18n/localization';
import { BooleanPicker } from '../pickers/boolean_picker';
import { NumberPicker } from '../pickers/number_picker';
import { ProgressTrackerModal } from '../progress_tracker_modal';

const WEB_DEFAULT_ITERATIONS = 5_000;
const WEB_ITERATIONS_LIMIT = 100_000;
const LOCAL_ITERATIONS_LIMIT = 5_000_000;

const WEB_COMBINATIONS_LIMIT = 50_000;
const LOCAL_COMBINATIONS_LIMIT = 100_000;

export interface TopGearResult {
	gear: Gear;
	dpsMetrics: DistributionMetrics;
}

export class BulkTab extends SimTab {
	readonly simUI: IndividualSimUI<any>;
	readonly playerCanDualWield: boolean;

	readonly itemsChangedEmitter = new TypedEvent<void>();
	readonly settingsChangedEmitter = new TypedEvent<void>();

	private readonly setupTabElem: HTMLElement;
	private readonly resultsTabElem: HTMLElement;
	private readonly combinationsElem: HTMLElement;
	private readonly bulkSimButton: HTMLButtonElement;
	private readonly settingsContainer: HTMLElement;

	private resultsTab: Tab;
	protected progressTrackerModal: ProgressTrackerModal;

	readonly selectorModal: SelectorModal;

	// The main array we will use to store items with indexes. Null values are the result of removed items to avoid having to shift pickers over and over.
	protected items: Array<ItemSpec | null> = new Array<ItemSpec | null>();
	protected pickerGroups: Map<BulkSimItemSlot, BulkItemPickerGroup> = new Map();

	protected simStart: number = 0;
	protected combinations = 0;
	protected iterations = 0;
	protected isRunning: boolean = false;
	protected isCancelling = false;
	protected bulkSimAbortController: AbortController | null = null;

	// Optimize each combination's gems (socket bonuses, meta-gem conditions, stat caps) with the
	// same LP engine as the "Suggest Gems" feature, instead of just filling sockets with the
	// fallback gems. In this MoP-derived code that engine also does reforging, which is a no-op in
	// TBC, so here it is effectively a per-combo gem optimizer. Off = fast crude fallback-gem fill.
	protected optimizeGems = true;

	// Iterations to run per combination. 0 means "use the default". Batch runs are a
	// ranking pass, so this is intentionally lower than a final single-sim count.
	protected batchIterations = 0;

	// Enchant options to try per item slot (enchant effectIds). Each slot with options becomes (or
	// extends) a combination dimension: the slot's item(s) are cross-producted with these enchants.
	protected enchantOptions: Map<ItemSlot, number[]> = new Map();

	frozenItems: Map<BulkSimItemSlot, EquippedItem | null> = new Map([
		[BulkSimItemSlot.ItemSlotFinger, null],
		[BulkSimItemSlot.ItemSlotTrinket, null],
	]);
	frozenWeaponSlot: ItemSlot.ItemSlotMainHand | ItemSlot.ItemSlotOffHand | undefined = undefined;
	weaponTypeFilters: Map<ItemSlot.ItemSlotMainHand | ItemSlot.ItemSlotOffHand, WeaponType[]> = new Map([
		[ItemSlot.ItemSlotMainHand, []],
		[ItemSlot.ItemSlotOffHand, []],
	]);
	fallbackGems: SimGem[];
	gemIconElements: HTMLImageElement[];

	protected topGearResults: TopGearResult[] | null = null;
	protected originalGear: Gear | null = null;
	protected originalGearResults: TopGearResult | null = null;

	constructor(parentElem: HTMLElement, simUI: IndividualSimUI<any>) {
		super(parentElem, simUI, { identifier: 'bulk-tab', title: i18n.t('bulk_tab.title') });

		this.simUI = simUI;
		this.playerCanDualWield = this.simUI.player.getPlayerSpec().canDualWield;

		const setupTabBtnRef = ref<HTMLButtonElement>();
		const setupTabRef = ref<HTMLDivElement>();
		const resultsTabBtnRef = ref<HTMLButtonElement>();
		const resultsTabRef = ref<HTMLDivElement>();
		const settingsContainerRef = ref<HTMLDivElement>();
		const combinationsElemRef = ref<HTMLHeadingElement>();
		const bulkSimBtnRef = ref<HTMLButtonElement>();

		this.contentContainer.appendChild(
			<>
				<div className="bulk-tab-left tab-panel-left">
					<div className="bulk-tab-tabs">
						<ul className="nav nav-tabs" attributes={{ role: 'tablist' }}>
							<li className="nav-item" attributes={{ role: 'presentation' }}>
								<button
									className="nav-link active"
									type="button"
									attributes={{
										role: 'tab',
										// @ts-expect-error
										'aria-controls': 'bulkSetupTab',
										'aria-selected': true,
									}}
									dataset={{
										bsToggle: 'tab',
										bsTarget: `#bulkSetupTab`,
									}}
									ref={setupTabBtnRef}>
									{i18n.t('bulk_tab.tabs.setup')}
								</button>
							</li>
							<li className="nav-item" attributes={{ role: 'presentation' }}>
								<button
									className="nav-link"
									type="button"
									attributes={{
										role: 'tab',
										// @ts-expect-error
										'aria-controls': 'bulkResultsTab',
										'aria-selected': false,
									}}
									dataset={{
										bsToggle: 'tab',
										bsTarget: `#bulkResultsTab`,
									}}
									ref={resultsTabBtnRef}>
									{i18n.t('bulk_tab.tabs.results')}
								</button>
							</li>
						</ul>
						<div className="tab-content">
							<div id="bulkSetupTab" className="tab-pane fade active show" ref={setupTabRef} />
							<div id="bulkResultsTab" className="tab-pane fade show" ref={resultsTabRef}>
								<div className="d-flex align-items-center justify-content-center p-gap">{i18n.t('bulk_tab.results.run_simulation')}</div>
							</div>
						</div>
					</div>
				</div>
				<div className="bulk-tab-right tab-panel-right">
					<div className="bulk-settings-outer-container">
						<div className="bulk-settings-container" ref={settingsContainerRef}>
							<div className="bulk-combinations-count h4" ref={combinationsElemRef} />
							<button className="btn btn-primary bulk-settings-btn" ref={bulkSimBtnRef}>
								{i18n.t('bulk_tab.actions.simulate_batch')}
							</button>
						</div>
					</div>
				</div>
			</>,
		);

		this.setupTabElem = setupTabRef.value!;
		this.resultsTabElem = resultsTabRef.value!;

		this.combinationsElem = combinationsElemRef.value!;
		this.bulkSimButton = bulkSimBtnRef.value!;
		this.settingsContainer = settingsContainerRef.value!;

		new Tab(setupTabBtnRef.value!);
		this.resultsTab = new Tab(resultsTabBtnRef.value!);

		this.selectorModal = new SelectorModal(this.simUI.rootElem, this.simUI, this.simUI.player, undefined, {
			id: 'bulk-selector-modal',
		});

		this.progressTrackerModal = new ProgressTrackerModal(simUI.rootElem, {
			id: 'bulk-sim-progress-tracker',
			title: 'Bulk Sim',
			hasProgressBar: true,
			onCancel: () => {
				this.abortBulkSim();
			},
		});

		this.fallbackGems = Array.from({ length: 5 }, () => UIGem.create());
		this.gemIconElements = [];

		this.buildTabContent();

		this.simUI.sim.waitForInit().then(() => {
			this.loadSettings();
			const loadEquippedItems = () => {
				if (this.isRunning) {
					return;
				}

				// Clear all previously equipped items from the pickers
				for (const group of this.pickerGroups.values()) {
					if (group.has(-1)) {
						group.remove(-1, true);
					}
					if (group.has(-2)) {
						group.remove(-2, true);
					}
				}

				this.simUI.player.getEquippedItems().forEach((equippedItem, slot) => {
					const bulkSlot = getBulkItemSlotFromSlot(slot, this.playerCanDualWield);
					const group = this.pickerGroups.get(bulkSlot)!;
					const idx = this.isSecondaryItemSlot(slot) ? -2 : -1;
					if (equippedItem) {
						group.add(idx, equippedItem, true);
					}
				});

				this.itemsChangedEmitter.emit(TypedEvent.nextEventID());
			};
			const updateCombinationsCount = () => {
				this.combinationsElem.replaceChildren(this.getCombinationsCount());
			};

			this.simUI.player.gearChangeEmitter.on(() => loadEquippedItems());

			TypedEvent.onAny([this.settingsChangedEmitter, this.itemsChangedEmitter]).on(() => this.storeSettings());
			TypedEvent.onAny([this.itemsChangedEmitter, this.settingsChangedEmitter, this.simUI.sim.iterationsChangeEmitter]).on(() =>
				updateCombinationsCount(),
			);

			loadEquippedItems();
			updateCombinationsCount();
		});
	}

	private getSettingsKey(): string {
		return this.simUI.getStorageKey('bulk-settings.v1');
	}

	private loadSettings() {
		const storedSettings = window.localStorage.getItem(this.getSettingsKey());
		if (storedSettings != null) {
			let settings: BulkSettings;
			try {
				settings = BulkSettings.fromJsonString(storedSettings, {
					ignoreUnknownFields: true,
				});
			} catch {
				settings = BulkSettings.create();
			}

			this.addItems(settings.items, true);
			this.batchIterations = settings.iterationsPerCombo;
			this.setFrozenItem(BulkSimItemSlot.ItemSlotFinger, this.getEquippedItemForFrozenSlot(BulkSimItemSlot.ItemSlotFinger, settings.freezeRingSlot));
			this.setFrozenItem(BulkSimItemSlot.ItemSlotTrinket, this.getEquippedItemForFrozenSlot(BulkSimItemSlot.ItemSlotTrinket, settings.freezeTrinketSlot));
			this.setFrozenWeaponSlot(settings.freezeWeaponSlot);
			this.setWeaponTypeFilter(ItemSlot.ItemSlotMainHand, settings.freezeMainhandWeaponSlots);
			this.setWeaponTypeFilter(ItemSlot.ItemSlotOffHand, settings.freezeOffhandWeaponSlots);
			this.fallbackGems = new Array<SimGem>(
				SimGem.create({ id: settings.defaultRedGem }),
				SimGem.create({ id: settings.defaultYellowGem }),
				SimGem.create({ id: settings.defaultBlueGem }),
				SimGem.create({ id: settings.defaultMetaGem }),
				SimGem.create({ id: settings.defaultPrismaticGem }),
			);

			this.fallbackGems.forEach((gem, idx) => {
				ActionId.fromItemId(gem.id)
					.fill()
					.then(filledId => {
						if (gem.id) {
							this.gemIconElements[idx].src = filledId.iconUrl;
							this.gemIconElements[idx].classList.remove('hide');
						}
					});
			});

			// The pickers were built (synchronously) before this deferred load ran, so they're showing
			// construction-time defaults. Emit so they re-read the values we just loaded from storage.
			this.settingsChangedEmitter.emit(TypedEvent.nextEventID());
		}
	}

	private storeSettings() {
		const settings = this.createBulkSettings();
		const setStr = BulkSettings.toJsonString(settings, { enumAsInteger: true });
		try {
			window.localStorage.setItem(this.getSettingsKey(), setStr);
		} catch (e) {
			if (e && e instanceof DOMException && e.name === 'QuotaExceededError') {
				window.localStorage.removeItem(this.getSettingsKey());
			}
		}
	}

	protected createBulkSettings(): BulkSettings {
		// Gem optimizer inputs: the player's EP weights, the Suggest Gems hard stat caps, and the
		// soft-cap breakpoints (e.g. hunter's 9% ranged-hit) as percent caps the backend converts to
		// per-combo rating caps. Falls back to the spec defaults when the reforger isn't instantiated.
		const statCaps = this.simUI.reforger?.statCaps ?? this.simUI.individualConfig.defaults.statCaps;
		const softCaps = this.simUI.reforger?.softCapsConfig ?? this.simUI.individualConfig.defaults.softCapBreakpoints ?? [];
		const gemCaps: GemStatCap[] = [];
		// Soft-cap breakpoints, sent as full piecewise curves (breakpoints + post-cap EPs).
		for (const cap of softCaps) {
			if (!cap.breakpoints.length) continue;
			gemCaps.push(
				GemStatCap.create({
					unitStat: cap.unitStat.isPseudoStat() ? cap.unitStat.getPseudoStat() : cap.unitStat.getStat(),
					isPseudostat: cap.unitStat.isPseudoStat(),
					breakpoints: cap.breakpoints.slice(),
					postCapEps: cap.postCapEPs.slice(),
				}),
			);
		}
		// Hard stat caps: one breakpoint with 0 EP past it.
		const hard = statCaps?.toProto();
		(hard?.stats ?? []).forEach((v, i) => {
			if (v > 0) gemCaps.push(GemStatCap.create({ unitStat: i, isPseudostat: false, breakpoints: [v], postCapEps: [0] }));
		});
		(hard?.pseudoStats ?? []).forEach((v, i) => {
			if (v > 0) gemCaps.push(GemStatCap.create({ unitStat: i, isPseudostat: true, breakpoints: [v], postCapEps: [0] }));
		});
		return BulkSettings.create({
			items: this.getItems(),
			defaultRedGem: this.fallbackGems[0].id,
			defaultYellowGem: this.fallbackGems[1].id,
			defaultBlueGem: this.fallbackGems[2].id,
			defaultMetaGem: this.fallbackGems[3].id,
			defaultPrismaticGem: this.fallbackGems[4].id,
			// Persist the raw user override (0 = "use the default", which tracks the global iteration
			// count). Storing the resolved value would freeze the default and stop it tracking.
			iterationsPerCombo: this.batchIterations,
			freezeRingSlot: this.getFrozenItemSlot(BulkSimItemSlot.ItemSlotFinger),
			freezeTrinketSlot: this.getFrozenItemSlot(BulkSimItemSlot.ItemSlotTrinket),
			freezeWeaponSlot: this.frozenWeaponSlot,
			freezeMainhandWeaponSlots: this.weaponTypeFilters.get(ItemSlot.ItemSlotMainHand)?.slice(),
			freezeOffhandWeaponSlots: this.weaponTypeFilters.get(ItemSlot.ItemSlotOffHand)?.slice(),
			optimizeGems: this.optimizeGems,
			gemEpWeights: this.simUI.player.getEpWeights().toProto().stats,
			gemCaps,
			disableUniqueGems: this.simUI.reforger?.disableUniqueGemsSetting ?? false,
			gemPoolIds: this.optimizeGems ? this.candidateGems().map(gem => gem.id) : [],
			...this.metaConditionSettings(),
		});
	}

	// The effective meta gem's color-activation condition, so the backend LP can keep the meta active
	// (same source of truth as Suggest Gems: the configured fallback meta gem, else the equipped one).
	private metaConditionSettings(): Partial<BulkSettings> {
		let metaGemId = this.fallbackGems[3]?.id ?? 0;
		if (!metaGemId) metaGemId = this.simUI.player.getGear().getMetaGem()?.id ?? 0;
		if (!metaGemId) return {};
		try {
			const cond = getMetaGemCondition(metaGemId);
			return {
				metaMinRed: cond.minRed,
				metaMinYellow: cond.minYellow,
				metaMinBlue: cond.minBlue,
				metaCompareColorGreater: cond.compareColorGreater,
				metaCompareColorLesser: cond.compareColorLesser,
			};
		} catch {
			return {}; // no known condition for this meta gem
		}
	}

	private getDefaultIterationsCount(): number {
		if (isExternal()) return WEB_DEFAULT_ITERATIONS;

		// Batch is a ranking pass, so default below the full single-sim precision,
		// but never above the user's configured iteration count.
		return Math.min(this.simUI.sim.getIterations(), WEB_DEFAULT_ITERATIONS);
	}

	// Effective iterations to run for each combination.
	protected getComboIterations(): number {
		return this.batchIterations > 0 ? this.batchIterations : this.getDefaultIterationsCount();
	}

	protected createBulkItemsDatabase(): SimDatabase {
		const itemsDb = SimDatabase.create();
		for (const is of this.items.values()) {
			if (!is) continue;

			const item = this.simUI.sim.db.lookupItemSpec(is);
			if (!item) {
				throw new Error(`item with ID ${is.id} not found in database`);
			}
			itemsDb.items.push(SimItem.fromJson(UIItem.toJson(item.item), { ignoreUnknownFields: true }));

			const ieRpp = this.simUI.sim.db.getItemEffectRandPropPoints(item.ilvl);
			if (ieRpp) {
				itemsDb.itemEffectRandPropPoints.push(ItemEffectRandPropPoints.create(this.simUI.sim.db.getItemEffectRandPropPoints(item.ilvl)));
			}

			if (item.enchant) {
				itemsDb.enchants.push(
					SimEnchant.fromJson(UIEnchant.toJson(item.enchant), {
						ignoreUnknownFields: true,
					}),
				);
			}
			if (item.randomSuffix) {
				itemsDb.randomSuffixes.push(
					ItemRandomSuffix.fromJson(ItemRandomSuffix.toJson(item.randomSuffix), {
						ignoreUnknownFields: true,
					}),
				);
			}
			for (const gem of item.gems) {
				if (gem) {
					itemsDb.gems.push(SimGem.fromJson(UIGem.toJson(gem), { ignoreUnknownFields: true }));
				}
			}
		}
		for (const gem of this.fallbackGems) {
			if (gem.id > 0) {
				itemsDb.gems.push(gem);
			}
		}
		// Include the definitions for every enchant option so the backend can resolve them.
		for (const [slot, ids] of this.enchantOptions.entries()) {
			if (!ids.length) continue;
			const available = this.simUI.sim.db.getEnchants(slot);
			for (const id of ids) {
				const enchant = available.find(e => e.effectId === id);
				if (enchant) {
					itemsDb.enchants.push(SimEnchant.fromJson(UIEnchant.toJson(enchant), { ignoreUnknownFields: true }));
				}
			}
		}
		// Ship the candidate gem pool so the optimizer can trade off (e.g. socket a hit gem to reach a
		// cap, then agility), not just the equipped/fallback gems.
		if (this.optimizeGems) {
			for (const gem of this.candidateGems()) {
				itemsDb.gems.push(SimGem.fromJson(UIGem.toJson(gem), { ignoreUnknownFields: true }));
			}
		}
		return itemsDb;
	}

	// The gems the optimizer is allowed to use, filtered exactly like Suggest Gems' buildGemOptions:
	// non-meta, within the configured max gem phase, and jewelcrafter-only gems only for a jeweler.
	// Both the shipped gem database and the backend's gem_pool_ids derive from this single list so the
	// optimizer can never reach for a gem the player couldn't actually use.
	private candidateGems(): UIGem[] {
		const maxPhase = this.simUI.reforger?.maxGemPhaseSetting ?? Number.MAX_SAFE_INTEGER;
		const hasJC = this.simUI.player.hasProfession(Profession.Jewelcrafting);
		return this.simUI.sim.db.getGems().filter(gem => {
			if (gem.id <= 0) return false; // a 0-id "no gem" entry would leave sockets empty
			if (gem.color === GemColor.GemColorMeta) return false;
			if (gem.phase > maxPhase) return false;
			if (gem.requiredProfession === Profession.Jewelcrafting && !hasJC) return false;
			if (!gem.stats.some(stat => stat !== 0)) return false; // statless gems aren't worth socketing
			return true;
		});
	}

	// Add an item to its eligible bulk sim item slot(s). Mainly used for importing and search
	addItem(item: ItemSpec) {
		this.addItems([item]);
	}
	// Add items to their eligible bulk sim item slot(s). Mainly used for importing and search
	addItems(items: ItemSpec[], silent = false) {
		items.forEach(item => {
			const equippedItem = this.simUI.sim.db.lookupItemSpec(item)?.withDynamicStats();
			if (equippedItem) {
				getEligibleItemSlots(equippedItem.item).forEach(slot => {
					// Avoid duplicating rings/trinkets/weapons
					if (this.isSecondaryItemSlot(slot) || !canEquipItem(equippedItem.item, this.simUI.player.getPlayerSpec(), slot)) return;

					const idx = this.items.push(item) - 1;
					const bulkSlot = getBulkItemSlotFromSlot(slot, this.playerCanDualWield);
					const group = this.pickerGroups.get(bulkSlot)!;
					group.add(idx, equippedItem, silent);
				});
			}
		});

		this.itemsChangedEmitter.emit(TypedEvent.nextEventID());
	}
	// Add an item to a particular bulk sim item slot
	addItemToSlot(item: ItemSpec, bulkSlot: BulkSimItemSlot) {
		const equippedItem = this.simUI.sim.db.lookupItemSpec(item)?.withDynamicStats();
		if (equippedItem) {
			const eligibleItemSlots = getEligibleItemSlots(equippedItem.item);
			if (!canEquipItem(equippedItem.item, this.simUI.player.getPlayerSpec(), eligibleItemSlots[0])) return;

			const idx = this.items.push(item) - 1;
			const group = this.pickerGroups.get(bulkSlot)!;
			group.add(idx, equippedItem);
			this.itemsChangedEmitter.emit(TypedEvent.nextEventID());
		}
	}

	updateItem(idx: number, newItem: ItemSpec) {
		const equippedItem = this.simUI.sim.db.lookupItemSpec(newItem)?.withDynamicStats();
		if (equippedItem) {
			this.items[idx] = newItem;

			getEligibleItemSlots(equippedItem.item).forEach(slot => {
				// Avoid duplicating rings/trinkets/weapons
				if (this.isSecondaryItemSlot(slot) || !canEquipItem(equippedItem.item, this.simUI.player.getPlayerSpec(), slot)) return;

				const bulkSlot = getBulkItemSlotFromSlot(slot, this.playerCanDualWield);
				const group = this.pickerGroups.get(bulkSlot)!;
				group.update(idx, equippedItem);
			});
		}

		this.itemsChangedEmitter.emit(TypedEvent.nextEventID());
	}

	removeItem(item: ItemSpec) {
		for (let idx = 0; idx < this.items.length; idx++) {
			if (this.items[idx] && ItemSpec.equals(this.items[idx]!, item)) {
				this.removeItemByIndex(idx);
				return;
			}
		}
	}
	removeItemByIndex(idx: number, silent = false) {
		if (idx < 0 || this.items.length < idx || !this.items[idx]) {
			new Toast({
				variant: 'error',
				body: i18n.t('bulk_tab.notifications.failed_to_remove_item'),
			});
			return;
		}

		const item = this.items[idx]!;
		const equippedItem = this.simUI.sim.db.lookupItemSpec(item);
		if (equippedItem) {
			this.items[idx] = null;

			// Try to find the matching item within its eligible groups
			getEligibleItemSlots(equippedItem.item).forEach(slot => {
				if (!canEquipItem(equippedItem.item, this.simUI.player.getPlayerSpec(), slot)) return;
				const bulkSlot = getBulkItemSlotFromSlot(slot, this.playerCanDualWield);
				const group = this.pickerGroups.get(bulkSlot)!;

				if (group.has(idx)) {
					group.remove(idx, silent);
				}
			});
			this.itemsChangedEmitter.emit(TypedEvent.nextEventID());
		}
	}

	clearItems() {
		for (let idx = 0; idx < this.items.length; idx++) {
			this.removeItemByIndex(idx, true);
		}
		this.items = new Array<ItemSpec>();
		this.itemsChangedEmitter.emit(TypedEvent.nextEventID());
	}

	hasItem(item: ItemSpec) {
		return this.items.some(i => !!i && ItemSpec.equals(i, item));
	}

	getItems(): Array<ItemSpec> {
		const result = new Array<ItemSpec>();
		this.items.forEach(spec => {
			if (!spec) return;

			result.push(ItemSpec.clone(spec));
		});
		return result;
	}

	protected getAllWeaponCombos(): [EquippedItem | null, EquippedItem | null][] {
		const allWeaponCombos: [EquippedItem | null, EquippedItem | null][] = [];

		// First find any configured 2H weapons.
		let all2HWeapons: EquippedItem[] = [];

		for (const bulkItemSlot of [BulkSimItemSlot.ItemSlotMainHand, BulkSimItemSlot.ItemSlotHandWeapon]) {
			if (!this.pickerGroups.has(bulkItemSlot)) {
				continue;
			}

			const pickerGroup = this.pickerGroups.get(bulkItemSlot)!;
			const allItemOptions: EquippedItem[] = Array.from(pickerGroup.pickers.values()).map(picker => picker.item);
			all2HWeapons = all2HWeapons.concat(
				allItemOptions.filter(
					equippedItem =>
						![RangedWeaponType.RangedWeaponTypeUnknown, RangedWeaponType.RangedWeaponTypeWand].includes(equippedItem.item.rangedWeaponType) ||
						equippedItem.item.handType == HandType.HandTypeTwoHand,
				),
			);
		}

		for (const twoHandWeapon of all2HWeapons) {
			allWeaponCombos.push([twoHandWeapon, null]);
		}

		// Then loop through all pairs of MH and OH items.
		const mhGroup = this.pickerGroups.get(BulkSimItemSlot.ItemSlotMainHand);
		const ohGroup = this.pickerGroups.get(BulkSimItemSlot.ItemSlotOffHand);

		if (mhGroup?.pickers.size) {
			for (const mhItem of Array.from(mhGroup.pickers.values()).map(picker => picker.item)) {
				if (all2HWeapons.includes(mhItem)) {
					continue;
				}

				if (ohGroup?.pickers.size) {
					for (const ohItem of Array.from(ohGroup.pickers.values()).map(picker => picker.item)) {
						allWeaponCombos.push([mhItem, ohItem]);
					}
				} else {
					allWeaponCombos.push([mhItem, null]);
				}
			}
		} else if (ohGroup?.pickers.size) {
			for (const ohItem of Array.from(ohGroup.pickers.values()).map(picker => picker.item)) {
				allWeaponCombos.push([null, ohItem]);
			}
		}
		// Finally loop through all one-hand weapons. Double count these since they can go in either slot.
		const oneHandGroup = this.pickerGroups.get(BulkSimItemSlot.ItemSlotHandWeapon);

		if (oneHandGroup?.pickers.size) {
			const allOneHandWeapons: EquippedItem[] = Array.from(oneHandGroup.pickers.values())
				.map(picker => picker.item)
				.filter(item => !all2HWeapons.includes(item));

			for (let i = 0; i < allOneHandWeapons.length; i++) {
				if (allOneHandWeapons.slice(0, i).some((item: EquippedItem) => item.equals(allOneHandWeapons[i], true, true))) {
					continue;
				}

				for (let j = i + 1; j < allOneHandWeapons.length; j++) {
					if (allOneHandWeapons.slice(i + 1, j).some((item: EquippedItem) => item.equals(allOneHandWeapons[j], true, true))) {
						continue;
					}

					allWeaponCombos.push([allOneHandWeapons[i], allOneHandWeapons[j]]);

					if (!allOneHandWeapons[i].equals(allOneHandWeapons[j], true, true)) {
						allWeaponCombos.push([allOneHandWeapons[j], allOneHandWeapons[i]]);
					}
				}
			}
		}

		return allWeaponCombos.filter(([mhItem, ohItem]) => this.weaponComboMatchesSettings(mhItem, ohItem));
	}

	protected getItemsForCombo(comboIdx: number): Map<ItemSlot, EquippedItem> {
		const itemsForCombo = new Map<ItemSlot, EquippedItem>();

		// Deal with weapon combos first since they bridge multiple slots.
		const allWeaponPairs = this.getAllWeaponCombos();
		const numWeaponPairs = allWeaponPairs.length;

		if (numWeaponPairs > 0) {
			const weaponPairIdx = comboIdx % numWeaponPairs;
			comboIdx = Math.floor(comboIdx / numWeaponPairs);
			const weaponPairToUse = allWeaponPairs[weaponPairIdx];

			if (weaponPairToUse[0]) {
				itemsForCombo.set(ItemSlot.ItemSlotMainHand, weaponPairToUse[0]);
			}

			if (weaponPairToUse[1]) {
				itemsForCombo.set(ItemSlot.ItemSlotOffHand, weaponPairToUse[1]);
			}
		}

		for (const [bulkItemSlot, pickerGroup] of this.pickerGroups.entries()) {
			if (
				pickerGroup.pickers.size == 0 ||
				[BulkSimItemSlot.ItemSlotMainHand, BulkSimItemSlot.ItemSlotOffHand, BulkSimItemSlot.ItemSlotHandWeapon].includes(bulkItemSlot)
			) {
				continue;
			}

			const optionsForSlot: EquippedItem[] = Array.from(pickerGroup.pickers.values()).map(picker => picker.item);
			const numOptions = optionsForSlot.length;

			if ([BulkSimItemSlot.ItemSlotFinger, BulkSimItemSlot.ItemSlotTrinket].includes(bulkItemSlot)) {
				if (numOptions < 2) {
					throw `At least 2 items must be selected for ${translateBulkSlotName(bulkItemSlot)}`;
				}

				let pairsForSlot = getAllPairs(optionsForSlot);
				const frozenItem = this.frozenItems.get(bulkItemSlot);

				if (frozenItem) {
					pairsForSlot = optionsForSlot.filter(option => !frozenItem.equals(option)).map(option => [frozenItem, option]);
				}

				const numPairs = pairsForSlot.length;
				const pairIdx = comboIdx % numPairs;
				comboIdx = Math.floor(comboIdx / numPairs);
				const pairToUse = pairsForSlot[pairIdx];
				const slotsToUse = bulkSimItemSlotToItemSlotPairs.get(bulkItemSlot)!;
				itemsForCombo.set(slotsToUse[0], pairToUse[0]);
				itemsForCombo.set(slotsToUse[1], pairToUse[1]);
			} else {
				const optionIdx = comboIdx % numOptions;
				comboIdx = Math.floor(comboIdx / numOptions);
				itemsForCombo.set(bulkSimItemSlotToSingleItemSlot.get(bulkItemSlot)!, optionsForSlot[optionIdx]);
			}
		}

		return itemsForCombo;
	}

	private getFrozenWeaponItem(): EquippedItem | undefined {
		if (!this.frozenWeaponSlot) {
			return undefined;
		}

		return this.simUI.player.getGear().getEquippedItem(this.frozenWeaponSlot) || undefined;
	}

	private matchesWeaponTypeFilter(equippedItem: EquippedItem | null, slot: ItemSlot.ItemSlotMainHand | ItemSlot.ItemSlotOffHand): boolean {
		const filter = this.weaponTypeFilters.get(slot)!;
		if (filter.length === 0) {
			return true;
		}

		if (!equippedItem) {
			return false;
		}

		return equippedItem.item.weaponType > WeaponType.WeaponTypeUnknown && filter.includes(equippedItem.item.weaponType);
	}

	private weaponComboMatchesSettings(mhItem: EquippedItem | null, ohItem: EquippedItem | null): boolean {
		const frozenWeaponItem = this.getFrozenWeaponItem();

		if (this.frozenWeaponSlot === ItemSlot.ItemSlotMainHand && frozenWeaponItem && !mhItem?.equals(frozenWeaponItem)) {
			return false;
		}
		if (this.frozenWeaponSlot === ItemSlot.ItemSlotOffHand && frozenWeaponItem && !ohItem?.equals(frozenWeaponItem)) {
			return false;
		}

		return this.matchesWeaponTypeFilter(mhItem, ItemSlot.ItemSlotMainHand) && this.matchesWeaponTypeFilter(ohItem, ItemSlot.ItemSlotOffHand);
	}

	protected calculateBulkCombinations() {
		try {
			let numCombinations: number = this.getAllWeaponCombos().length;

			for (const [bulkItemSlot, pickerGroup] of this.pickerGroups.entries()) {
				if ([BulkSimItemSlot.ItemSlotMainHand, BulkSimItemSlot.ItemSlotOffHand, BulkSimItemSlot.ItemSlotHandWeapon].includes(bulkItemSlot)) {
					continue;
				}

				const numOptions: number = pickerGroup.pickers.size;

				if (numOptions > 1 && [BulkSimItemSlot.ItemSlotFinger, BulkSimItemSlot.ItemSlotTrinket].includes(bulkItemSlot)) {
					if (this.frozenItems.get(bulkItemSlot)) {
						numCombinations *= numOptions - 1;
					} else {
						numCombinations *= binomialCoefficient(numOptions, 2);
					}
				} else {
					numCombinations *= Math.max(numOptions, 1);
				}
			}

			// Each slot with enchant options multiplies the space by its option count (whether that
			// slot also varies items - item×enchant - or only varies the enchant on the equipped item).
			for (const [slot, ids] of this.enchantOptions.entries()) {
				if (ids.length) numCombinations *= this.enchantsToTry(slot).length;
			}

			this.combinations = numCombinations;
			this.iterations = this.getComboIterations() * numCombinations;
		} catch (e) {
			this.simUI.handleCrash(e);
		}
	}

	// Builds the combination space (per-slot option lists, weapon configs, ring/trinket pairs) as
	// proto dimensions for the backend. Mirrors calculateBulkCombinations, so the product of the
	// dimension sizes equals this.combinations. The backend expands the cartesian product and does
	// the heavy work (gemming, simming, ranking); this is just the cheap option lists.
	// enchantsToTry returns the enchants to try for a slot: the configured options resolved to
	// Enchant objects, or [null] (keep the item's own enchant) when none are configured.
	private enchantsToTry(slot: ItemSlot): (UIEnchant | null)[] {
		const ids = this.enchantOptions.get(slot);
		if (!ids || !ids.length) return [null];
		const available = this.simUI.sim.db.getEnchants(slot);
		const resolved = ids.map(id => available.find(e => e.effectId === id)).filter((e): e is UIEnchant => !!e);
		return resolved.length ? resolved : [null];
	}

	private buildComboDimensions(): BulkComboDimension[] {
		const dims: BulkComboDimension[] = [];
		const coveredSlots = new Set<ItemSlot>(); // item slots that already have a dimension

		// Weapon dimension (getAllWeaponCombos already includes the equipped weapons), cross-producted
		// with the main-hand and off-hand enchant options.
		const weaponCombos = this.getAllWeaponCombos();
		const mhEnchants = this.enchantsToTry(ItemSlot.ItemSlotMainHand);
		const ohEnchants = this.enchantsToTry(ItemSlot.ItemSlotOffHand);
		const weaponChoices: BulkComboChoice[] = [];
		for (const [mh, oh] of weaponCombos) {
			for (const mhEnchant of mhEnchants) {
				for (const ohEnchant of ohEnchants) {
					const items: BulkComboSlotItem[] = [];
					if (mh) items.push(BulkComboSlotItem.create({ slot: ItemSlot.ItemSlotMainHand, item: mhEnchant ? mh.withEnchant(mhEnchant).asSpec() : mh.asSpec() }));
					if (oh) items.push(BulkComboSlotItem.create({ slot: ItemSlot.ItemSlotOffHand, item: ohEnchant ? oh.withEnchant(ohEnchant).asSpec() : oh.asSpec() }));
					weaponChoices.push(BulkComboChoice.create({ items }));
				}
			}
		}
		dims.push(BulkComboDimension.create({ choices: weaponChoices }));
		coveredSlots.add(ItemSlot.ItemSlotMainHand);
		coveredSlots.add(ItemSlot.ItemSlotOffHand);

		for (const [bulkItemSlot, pickerGroup] of this.pickerGroups.entries()) {
			if (
				pickerGroup.pickers.size == 0 ||
				[BulkSimItemSlot.ItemSlotMainHand, BulkSimItemSlot.ItemSlotOffHand, BulkSimItemSlot.ItemSlotHandWeapon].includes(bulkItemSlot)
			) {
				continue;
			}

			const options = Array.from(pickerGroup.pickers.values()).map(picker => picker.item);
			const numOptions = options.length;

			if (numOptions > 1 && [BulkSimItemSlot.ItemSlotFinger, BulkSimItemSlot.ItemSlotTrinket].includes(bulkItemSlot)) {
				let pairs = getAllPairs(options);
				const frozenItem = this.frozenItems.get(bulkItemSlot);
				if (frozenItem) {
					pairs = options.filter(option => !frozenItem.equals(option)).map(option => [frozenItem, option] as [EquippedItem, EquippedItem]);
				}
				const slots = bulkSimItemSlotToItemSlotPairs.get(bulkItemSlot)!;
				coveredSlots.add(slots[0]);
				coveredSlots.add(slots[1]);
				const e0 = this.enchantsToTry(slots[0]);
				const e1 = this.enchantsToTry(slots[1]);
				const pairChoices: BulkComboChoice[] = [];
				for (const [a, b] of pairs) {
					for (const en0 of e0) {
						for (const en1 of e1) {
							pairChoices.push(
								BulkComboChoice.create({
									items: [
										BulkComboSlotItem.create({ slot: slots[0], item: en0 ? a.withEnchant(en0).asSpec() : a.asSpec() }),
										BulkComboSlotItem.create({ slot: slots[1], item: en1 ? b.withEnchant(en1).asSpec() : b.asSpec() }),
									],
								}),
							);
						}
					}
				}
				dims.push(BulkComboDimension.create({ choices: pairChoices }));
			} else {
				const slot = bulkSimItemSlotToSingleItemSlot.get(bulkItemSlot) ?? bulkSimItemSlotToItemSlotPairs.get(bulkItemSlot)![0];
				coveredSlots.add(slot);
				// Cross-product the item options with the enchant options for this slot.
				const enchants = this.enchantsToTry(slot);
				const choices: BulkComboChoice[] = [];
				for (const option of options) {
					for (const enchant of enchants) {
						const spec = enchant ? option.withEnchant(enchant).asSpec() : option.asSpec();
						choices.push(BulkComboChoice.create({ items: [BulkComboSlotItem.create({ slot, item: spec })] }));
					}
				}
				dims.push(BulkComboDimension.create({ choices }));
			}
		}

		// Slots with enchant options but no item dimension: vary just the enchant on the equipped item.
		for (const [slot, ids] of this.enchantOptions.entries()) {
			if (!ids.length || coveredSlots.has(slot)) continue;
			const item = this.simUI.player.getGear().getEquippedItem(slot);
			if (!item) continue;
			const enchants = this.enchantsToTry(slot);
			dims.push(
				BulkComboDimension.create({
					choices: enchants.map(enchant =>
						BulkComboChoice.create({
							items: [BulkComboSlotItem.create({ slot, item: enchant ? item.withEnchant(enchant).asSpec() : item.asSpec() })],
						}),
					),
				}),
			);
		}

		return dims;
	}

	protected buildTabContent() {
		this.buildSetupTabContent();
		this.buildResultsTabContent();
		this.buildBatchSettings();
	}

	private buildSetupTabContent() {
		const bagImportBtnRef = ref<HTMLButtonElement>();
		const favsImportBtnRef = ref<HTMLButtonElement>();
		const clearBtnRef = ref<HTMLButtonElement>();
		this.setupTabElem.appendChild(
			<>
				{/* // TODO: Remove once we're more comfortable with the state of Batch sim */}
				<p className="mb-0" innerHTML={i18n.t('bulk_tab.description')} />
				{isExternal() && (
					<p className="mb-0">
						<a href={REPO_RELEASES_URL} target="_blank">
							<i className="fas fa-gauge-high me-1" />
							{i18n.t('bulk_tab.download_local')}
						</a>
					</p>
				)}
				<div className="bulk-gear-actions">
					<button className="btn btn-secondary" ref={bagImportBtnRef}>
						<i className="fa fa-download me-1" /> {i18n.t('bulk_tab.actions.import_bags')}
					</button>
					<button className="btn btn-secondary" ref={favsImportBtnRef}>
						<i className="fa fa-download me-1" /> {i18n.t('bulk_tab.actions.import_favorites')}
					</button>
					<button className="btn btn-danger ms-auto" ref={clearBtnRef}>
						<i className="fas fa-times me-1" />
						{i18n.t('bulk_tab.actions.clear_items')}
					</button>
				</div>
			</>,
		);

		const bagImportButton = bagImportBtnRef.value!;
		const favsImportButton = favsImportBtnRef.value!;
		const clearButton = clearBtnRef.value!;

		bagImportButton.addEventListener('click', () => new BulkGearJsonImporter(this.simUI.rootElem, this.simUI, this).open());

		favsImportButton.addEventListener('click', () => {
			const filters = this.simUI.player.sim.getFilters();
			const items = filters.favoriteItems.map(itemID => ItemSpec.create({ id: itemID }));
			this.addItems(items);
		});

		clearButton.addEventListener('click', () => this.clearItems());

		new BulkItemSearch(this.setupTabElem, this.simUI, this);

		const itemList = (<div className="bulk-gear-combo" />) as HTMLElement;
		this.setupTabElem.appendChild(itemList);

		getEnumValues<BulkSimItemSlot>(BulkSimItemSlot).forEach(bulkSlot => {
			if (this.playerCanDualWield && [BulkSimItemSlot.ItemSlotMainHand, BulkSimItemSlot.ItemSlotOffHand].includes(bulkSlot)) return;
			if (!this.playerCanDualWield && bulkSlot === BulkSimItemSlot.ItemSlotHandWeapon) return;
			this.pickerGroups.set(bulkSlot, new BulkItemPickerGroup(itemList, this.simUI, this, bulkSlot));
		});
	}

	private resetResultsTabContent() {
		this.resultsTabElem.replaceChildren();
	}

	private buildResultsTabContent() {
		if (!this.topGearResults || !this.originalGearResults) {
			return;
		}

		for (const topGearResult of this.topGearResults) {
			new BulkSimResultRenderer(this.resultsTabElem, this.simUI, topGearResult, this.originalGearResults);
		}

		this.resultsTab.show();
	}

	// Return whether or not the slot is considered secondary and the item should be grouped
	// This includes items in the Finger2 or Trinket2 slots, or OffHand for dual-wield specs
	private isSecondaryItemSlot(slot: ItemSlot) {
		return isSecondaryItemSlot(slot) || (this.playerCanDualWield && slot === ItemSlot.ItemSlotOffHand);
	}

	private createFreezeWeaponTypePickers(container: HTMLElement, slot: ItemSlot.ItemSlotMainHand | ItemSlot.ItemSlotOffHand) {
		const weaponTypes = Array.from(
			new Set(
				this.simUI.player
					.getPlayerClass()
					.weaponTypes.filter(
						eligibleWeaponType =>
							slot === ItemSlot.ItemSlotMainHand ||
							(this.playerCanDualWield && ![WeaponType.WeaponTypePolearm, WeaponType.WeaponTypeStaff].includes(eligibleWeaponType.weaponType)),
					)
					.map(eligibleWeaponType => eligibleWeaponType.weaponType),
			),
		);

		if (!weaponTypes.length) return;

		const freezeWeaponTypeContainerRef = ref<HTMLDivElement>();
		const freezeWeaponTypeListRef = ref<HTMLDivElement>();

		container.appendChild(
			<div className={clsx('bulk-gear-freeze-weapontypes', this.frozenWeaponSlot === slot && 'hide')} ref={freezeWeaponTypeContainerRef}>
				<h6 className="mb-2">
					{slot === ItemSlot.ItemSlotMainHand
						? i18n.t('bulk_tab.settings.freeze_weapon_types.mainhand_label')
						: i18n.t('bulk_tab.settings.freeze_weapon_types.offhand_label')}
				</h6>
				<div className="fs-content mb-2">{i18n.t('bulk_tab.settings.freeze_weapon_types.tooltip')}</div>
				<div className="bulk-gear-freeze-weapontypes__list gap-1" ref={freezeWeaponTypeListRef}></div>
			</div>,
		);

		const updateVisibility = () => freezeWeaponTypeContainerRef.value?.parentElement?.classList.toggle('hide', this.frozenWeaponSlot === slot);
		const visibilityChange = this.settingsChangedEmitter.on(updateVisibility);
		this.addOnDisposeCallback(() => visibilityChange.dispose());

		weaponTypes.forEach(weaponType => {
			new BooleanPicker<BulkTab>(freezeWeaponTypeListRef.value!, this, {
				id: `bulk-${slot}-weapon-type-${weaponType}`,
				label: translateWeaponType(weaponType),
				inline: true,
				changedEvent: _modObj => this.settingsChangedEmitter,
				getValue: _modObj => this.weaponTypeFilters.get(slot)!.includes(weaponType),
				setValue: (eventID, _modObj, newValue: boolean) => {
					const filter = this.weaponTypeFilters.get(slot)!;
					this.setWeaponTypeFilter(slot, newValue ? [...filter, weaponType] : filter.filter(type => type !== weaponType), eventID);
				},
			});
		});
	}

	private setFrozenItem(
		bulkSlot: BulkSimItemSlot.ItemSlotFinger | BulkSimItemSlot.ItemSlotTrinket,
		item: EquippedItem | null,
		eventID = TypedEvent.nextEventID(),
	) {
		if (item === this.frozenItems.get(bulkSlot)) {
			return;
		}

		this.frozenItems.set(bulkSlot, item);
		this.settingsChangedEmitter.emit(eventID);
	}

	private getEquippedItemForFrozenSlot(bulkSlot: BulkSimItemSlot.ItemSlotFinger | BulkSimItemSlot.ItemSlotTrinket, itemSlot: number): EquippedItem | null {
		const slots = bulkSimItemSlotToItemSlotPairs.get(bulkSlot);
		if (!slots?.includes(itemSlot)) {
			return null;
		}

		return this.simUI.player.getGear().getEquippedItem(itemSlot) ?? null;
	}

	private getFrozenItemSlot(bulkSlot: BulkSimItemSlot.ItemSlotFinger | BulkSimItemSlot.ItemSlotTrinket): ItemSlot | undefined {
		const frozenItem = this.frozenItems.get(bulkSlot);
		const slots = bulkSimItemSlotToItemSlotPairs.get(bulkSlot);
		if (!frozenItem || !slots) {
			return undefined;
		}

		const currentGear = this.simUI.player.getGear();
		return (
			slots.find(slot => currentGear.getEquippedItem(slot) === frozenItem) ??
			slots.find(slot => currentGear.getEquippedItem(slot)?.equals(frozenItem)) ??
			undefined
		);
	}

	private setWeaponTypeFilter(
		slot: ItemSlot.ItemSlotMainHand | ItemSlot.ItemSlotOffHand,
		newFilter: WeaponType[],
		eventID = TypedEvent.nextEventID(),
		shouldEmit = true,
	): boolean {
		const currentFilter = this.weaponTypeFilters.get(slot)!;
		const hasChanged = currentFilter.length !== newFilter.length || currentFilter.some((weaponType, idx) => weaponType !== newFilter[idx]);

		if (!hasChanged) {
			return false;
		}

		this.weaponTypeFilters.set(slot, newFilter);
		if (shouldEmit) {
			this.settingsChangedEmitter.emit(eventID);
		}
		return true;
	}

	private clearWeaponTypeFilter(slot: ItemSlot.ItemSlotMainHand | ItemSlot.ItemSlotOffHand): boolean {
		return this.setWeaponTypeFilter(slot, [], undefined, false);
	}

	private setFrozenWeaponSlot(itemSlot: number | null, eventID = TypedEvent.nextEventID()): boolean {
		const newSlot = [ItemSlot.ItemSlotMainHand, ItemSlot.ItemSlotOffHand].includes(itemSlot ?? -1)
			? (itemSlot as ItemSlot.ItemSlotMainHand | ItemSlot.ItemSlotOffHand)
			: undefined;
		const filtersChanged = newSlot !== undefined && this.clearWeaponTypeFilter(newSlot);

		if (newSlot === this.frozenWeaponSlot && !filtersChanged) {
			return false;
		}

		this.frozenWeaponSlot = newSlot;
		this.settingsChangedEmitter.emit(eventID);
		return true;
	}

	protected buildBatchSettings() {
		this.bulkSimButton.addEventListener('click', () => this.runBatchSim());

		const socketsContainerRef = ref<HTMLDivElement>();
		const frozenRingDiv = ref<HTMLDivElement>();
		const frozenTrinketDiv = ref<HTMLDivElement>();
		const frozenWeaponDiv = ref<HTMLDivElement>();
		const mainHandWeaponTypesDiv = ref<HTMLDivElement>();
		const offHandWeaponTypesDiv = ref<HTMLDivElement>();

		this.settingsContainer.appendChild(
			<>
				<div className="fallback-gem-container">
					<h6>{i18n.t('bulk_tab.settings.fallback_gems')}</h6>
					<div ref={socketsContainerRef} className="sockets-container"></div>
				</div>
				<div ref={frozenRingDiv}></div>
				<div ref={frozenTrinketDiv}></div>
				{this.playerCanDualWield && (
					<>
						<div ref={frozenWeaponDiv}></div>
						<div ref={mainHandWeaponTypesDiv}></div>
						<div ref={offHandWeaponTypesDiv}></div>
					</>
				)}
			</>,
		);

		if (frozenRingDiv.value)
			new EnumPicker<BulkTab>(frozenRingDiv.value, this, {
				id: 'freeze-ring',
				label: i18n.t('bulk_tab.settings.freeze_ring.label'),
				labelTooltip: i18n.t('bulk_tab.settings.freeze_ring.tooltip'),
				values: [
					{ name: i18n.t('common.none'), value: -1 },
					{ name: i18n.t('slots.finger_1', { ns: 'character' }), value: ItemSlot.ItemSlotFinger1 },
					{ name: i18n.t('slots.finger_2', { ns: 'character' }), value: ItemSlot.ItemSlotFinger2 },
				],
				changedEvent: _modObj => TypedEvent.onAny([this.settingsChangedEmitter, this.itemsChangedEmitter]),
				getValue: _modObj => {
					const frozenRing = this.frozenItems.get(BulkSimItemSlot.ItemSlotFinger);

					if (!frozenRing) {
						return -1;
					}

					const currentGear: Gear = this.simUI.player.getGear();

					if (currentGear.getEquippedItem(ItemSlot.ItemSlotFinger1)?.equals(frozenRing)) {
						return ItemSlot.ItemSlotFinger1;
					} else if (currentGear.getEquippedItem(ItemSlot.ItemSlotFinger2)?.equals(frozenRing)) {
						return ItemSlot.ItemSlotFinger2;
					} else {
						this.setFrozenItem(BulkSimItemSlot.ItemSlotFinger, null);
						return -1;
					}
				},
				setValue: (eventID, _modObj, newValue) => {
					let newItem: EquippedItem | null = null;

					if (newValue != -1) {
						newItem = this.simUI.player.getGear().getEquippedItem(newValue);
					}

					this.setFrozenItem(BulkSimItemSlot.ItemSlotFinger, newItem, eventID);
				},
			});

		if (frozenTrinketDiv.value)
			new EnumPicker<BulkTab>(frozenTrinketDiv.value, this, {
				id: 'freeze-trinket',
				label: i18n.t('bulk_tab.settings.freeze_trinket.label'),
				labelTooltip: i18n.t('bulk_tab.settings.freeze_trinket.tooltip'),
				values: [
					{ name: i18n.t('common.none'), value: -1 },
					{ name: i18n.t('slots.trinket_1', { ns: 'character' }), value: ItemSlot.ItemSlotTrinket1 },
					{ name: i18n.t('slots.trinket_2', { ns: 'character' }), value: ItemSlot.ItemSlotTrinket2 },
				],
				changedEvent: _modObj => TypedEvent.onAny([this.settingsChangedEmitter, this.itemsChangedEmitter]),
				getValue: _modObj => {
					const frozenTrinket = this.frozenItems.get(BulkSimItemSlot.ItemSlotTrinket);

					if (!frozenTrinket) {
						return -1;
					}

					const currentGear: Gear = this.simUI.player.getGear();

					if (currentGear.getEquippedItem(ItemSlot.ItemSlotTrinket1)?.equals(frozenTrinket)) {
						return ItemSlot.ItemSlotTrinket1;
					} else if (currentGear.getEquippedItem(ItemSlot.ItemSlotTrinket2)?.equals(frozenTrinket)) {
						return ItemSlot.ItemSlotTrinket2;
					} else {
						this.setFrozenItem(BulkSimItemSlot.ItemSlotTrinket, null);
						return -1;
					}
				},
				setValue: (eventID, _modObj, newValue) => {
					let newItem: EquippedItem | null = null;

					if (newValue != -1) {
						newItem = this.simUI.player.getGear().getEquippedItem(newValue);
					}

					this.setFrozenItem(BulkSimItemSlot.ItemSlotTrinket, newItem, eventID);
				},
			});

		if (this.playerCanDualWield) {
			if (frozenWeaponDiv.value)
				new EnumPicker<BulkTab>(frozenWeaponDiv.value, this, {
					id: 'freeze-weapon',
					label: i18n.t('bulk_tab.settings.freeze_weapon.label'),
					labelTooltip: i18n.t('bulk_tab.settings.freeze_weapon.tooltip'),
					values: [
						{ name: i18n.t('common.none'), value: -1 },
						{ name: i18n.t('slots.main_hand', { ns: 'character' }), value: ItemSlot.ItemSlotMainHand },
						{ name: i18n.t('slots.off_hand', { ns: 'character' }), value: ItemSlot.ItemSlotOffHand },
					],
					changedEvent: _modObj => TypedEvent.onAny([this.settingsChangedEmitter, this.itemsChangedEmitter]),
					getValue: _modObj => {
						if (!this.frozenWeaponSlot) {
							return -1;
						}

						return this.frozenWeaponSlot;
					},
					setValue: (eventID, _modObj, newValue) => {
						this.setFrozenWeaponSlot(newValue === -1 ? null : newValue, eventID);
					},
				});

			if (mainHandWeaponTypesDiv.value) this.createFreezeWeaponTypePickers(mainHandWeaponTypesDiv.value, ItemSlot.ItemSlotMainHand);
			if (offHandWeaponTypesDiv.value) this.createFreezeWeaponTypePickers(offHandWeaponTypesDiv.value, ItemSlot.ItemSlotOffHand);
		}

		Array<GemColor>(GemColor.GemColorRed, GemColor.GemColorYellow, GemColor.GemColorBlue, GemColor.GemColorMeta, GemColor.GemColorPrismatic).forEach(
			(socketColor, socketIndex) => {
				const gemContainerRef = ref<HTMLDivElement>();
				const gemIconRef = ref<HTMLImageElement>();
				const socketIconRef = ref<HTMLImageElement>();

				socketsContainerRef.value!.appendChild(
					<div ref={gemContainerRef} className="gem-socket-container">
						<img ref={gemIconRef} className="gem-icon hide" />
						<img ref={socketIconRef} className="socket-icon" />
					</div>,
				);

				this.gemIconElements.push(gemIconRef.value!);
				socketIconRef.value!.src = getEmptyGemSocketIconUrl(socketColor);

				let selector: GemSelectorModal;

				const onSelectHandler = (itemData: ItemData<UIGem>) => {
					this.fallbackGems[socketIndex] = itemData.item;
					this.storeSettings();
					ActionId.fromItemId(itemData.id)
						.fill()
						.then(filledId => {
							if (itemData.id) {
								this.gemIconElements[socketIndex].src = filledId.iconUrl;
								this.gemIconElements[socketIndex].classList.remove('hide');
							}
						});
					selector.close();
				};

				const onRemoveHandler = () => {
					this.fallbackGems[socketIndex] = UIGem.create();
					this.storeSettings();
					this.gemIconElements[socketIndex].classList.add('hide');
					this.gemIconElements[socketIndex].src = '';
					selector.close();
				};

				const openGemSelector = () => {
					if (!selector) selector = new GemSelectorModal(this.simUI.rootElem, this.simUI, socketColor, onSelectHandler, onRemoveHandler);
					selector.show();
				};

				this.gemIconElements[socketIndex].addEventListener('click', openGemSelector);
				gemContainerRef.value?.addEventListener('click', openGemSelector);
			},
		);

		// Sit the iterations input right under the Simulate button (they belong together, like the
		// global iterations input) instead of buried below the gem/enchant settings.
		const iterationsContainer = document.createElement('div');
		this.bulkSimButton.insertAdjacentElement('afterend', iterationsContainer);
		new NumberPicker<BulkTab>(iterationsContainer, this, {
			id: 'bulk-iterations-per-combo',
			label: 'Iterations per combo',
			labelTooltip:
				'How many iterations to sim each gear combination at. Total work is this number times the number of combinations, so lowering it speeds up the whole batch proportionally. Use a low value (e.g. 1000) for a quick ranking pass, then re-sim the top results at full iterations.',
			inline: true,
			changedEvent: _modObj => this.settingsChangedEmitter,
			getValue: _modObj => this.getComboIterations(),
			setValue: (eventID, _modObj, newValue: number) => {
				this.batchIterations = Math.max(1, Math.floor(newValue));
				this.settingsChangedEmitter.emit(eventID);
			},
		});

		const optimizeGemsContainer = document.createElement('div');
		this.settingsContainer.appendChild(optimizeGemsContainer);
		new BooleanPicker<BulkTab>(optimizeGemsContainer, this, {
			id: 'bulk-optimize-gems',
			label: 'Optimize gems',
			labelTooltip:
				'Re-gems each combination optimally — socket bonuses, meta-gem requirements and stat caps — using the same engine as Suggest Gems, instead of just filling every socket with your fallback gems. Much more accurate, but adds time per combination (a stat calc + solve each). Turn off for a fast crude fill.',
			inline: true,
			changedEvent: _modObj => this.settingsChangedEmitter,
			getValue: _modObj => this.optimizeGems,
			setValue: (eventID, _modObj, newValue: boolean) => {
				this.optimizeGems = newValue;
				this.settingsChangedEmitter.emit(eventID);
			},
		});

		// Enchant options: pick enchants to try per slot. Each selected enchant becomes extra
		// combinations (the slot's item(s) crossed with the chosen enchants). Phase 1 covers the
		// single-item armor slots; weapons/rings/trinkets are not yet supported.
		const enchantsContainer = document.createElement('div');
		enchantsContainer.classList.add('bulk-settings-enchants', 'mt-2');
		const enchantsHeading = document.createElement('label');
		enchantsHeading.classList.add('form-label');
		enchantsHeading.textContent = 'Enchant options to try (Ctrl/Cmd-click to pick several per slot)';
		enchantsContainer.appendChild(enchantsHeading);
		this.settingsContainer.appendChild(enchantsContainer);
		// Populate the per-slot enchant selects once the item database has loaded (it's async, so
		// this.simUI.sim.db is null while the tab is first built).
		this.simUI.sim.waitForInit().then(() => {
			const enchantableSlots = new Set<ItemSlot>([
				...bulkSimItemSlotToSingleItemSlot.values(),
				...Array.from(bulkSimItemSlotToItemSlotPairs.values()).flat(),
			]);
			for (const slot of enchantableSlots) {
				const enchants = this.simUI.sim.db.getEnchants(slot);
				if (!enchants.length) continue;
				const row = document.createElement('div');
				row.classList.add('d-flex', 'align-items-center', 'gap-2', 'mb-1');
				const label = document.createElement('span');
				label.style.minWidth = '5rem';
				label.textContent = ItemSlot[slot].replace('ItemSlot', '');
				const select = document.createElement('select');
				select.multiple = true;
				select.classList.add('form-select', 'form-select-sm');
				for (const enchant of enchants) {
					const opt = document.createElement('option');
					opt.value = String(enchant.effectId);
					opt.textContent = enchant.name;
					select.appendChild(opt);
				}
				select.addEventListener('change', () => {
					const ids = Array.from(select.selectedOptions).map(o => Number(o.value));
					if (ids.length) this.enchantOptions.set(slot, ids);
					else this.enchantOptions.delete(slot);
					this.settingsChangedEmitter.emit(TypedEvent.nextEventID());
				});
				row.appendChild(label);
				row.appendChild(select);
				enchantsContainer.appendChild(row);
			}
		});
	}

	private getCombinationsCount(): Element {
		this.calculateBulkCombinations();
		this.bulkSimButton.disabled = !this.combinations || this.combinations > this.getCombinationsLimit();

		// Big counts are hard to read, so group with thousand separators and tack on a compact
		// abbreviation (10K / 1.2M / 3B / 1T). The abbreviation is dropped below 10,000, where it adds nothing.
		const grouped = (n: number) => formatToNumber(n, { maximumFractionDigits: 0 });
		const compact = (n: number) => (n >= 10000 ? ` (${formatToCompactNumber(n, { maximumFractionDigits: 1 })})` : '');

		const warningRef = ref<HTMLButtonElement>();
		const rtn = (
			<>
				<span className={clsx(this.showIterationsWarning() && 'text-danger')}>
					{this.combinations === 1
						? i18n.t('bulk_tab.settings.combination_singular')
						: `${i18n.t('bulk_tab.settings.combinations_count', { value: grouped(this.combinations) })}${compact(this.combinations)}`}
					<br />
					<small>
						{grouped(this.iterations)} {i18n.t('bulk_tab.settings.iterations')}
						{compact(this.iterations)}
					</small>
				</span>
				{this.showIterationsWarning() && (
					<button className="warning link-warning" ref={warningRef}>
						<i className="fas fa-exclamation-triangle fa-2x" />
					</button>
				)}
			</>
		);

		if (warningRef.value) {
			tippy(warningRef.value, {
				content: i18n.t('bulk_tab.warning.iterations_limit', { limit: this.getIterationsLimit() }),
				placement: 'left',
				popperOptions: {
					modifiers: [
						{
							name: 'flip',
							options: {
								fallbackPlacements: ['auto'],
							},
						},
					],
				},
			});
		}

		return rtn;
	}

	private showIterationsWarning(): boolean {
		return this.iterations > this.getIterationsLimit();
	}

	private getIterationsLimit(): number {
		return isExternal() ? WEB_ITERATIONS_LIMIT : LOCAL_ITERATIONS_LIMIT;
	}

	private getCombinationsLimit(): number {
		return isExternal() ? WEB_COMBINATIONS_LIMIT : LOCAL_COMBINATIONS_LIMIT;
	}

	private setReforgeProgress(currentRound: number, rounds: number) {
		this.progressTrackerModal.updateProgress({
			stage: 'reforging',
			title: 'Optimizing gems',
			current: currentRound - 1,
			total: rounds,
			message: undefined,
		});
	}

	private setSimProgress(progress: ProgressMetrics, currentRound: number, rounds: number) {
		const isBaselineRound = currentRound === 1;
		const totalElapsedSeconds = (new Date().getTime() - this.simStart) / 1000;
		const roundFraction = progress.totalIterations > 0 ? progress.completedIterations / progress.totalIterations : 0;
		const completedRounds = Math.max(0, currentRound - 1 + roundFraction);
		const roundsRemaining = Math.max(0, rounds - completedRounds);
		const secondsRemaining = completedRounds > 0 ? (totalElapsedSeconds / completedRounds) * roundsRemaining : 0;

		if (isNaN(Number(secondsRemaining))) return;

		this.progressTrackerModal.updateProgress({
			stage: 'sim',
			title: isBaselineRound ? i18n.t('bulk_tab.progress.baseline_round') : i18n.t('bulk_tab.progress.refining_rounds'),
			current: currentRound - 1 + roundFraction,
			total: rounds,
			message: (
				<div className="results-sim">
					<div
						innerHTML={i18n.t('bulk_tab.progress.iterations_complete', {
							completed: progress.completedIterations,
							total: progress.totalIterations,
						})}
					/>
					<div>{i18n.t('bulk_tab.progress.seconds_remaining', { time: formatDuration(secondsRemaining) })}</div>
				</div>
			),
		});
	}

	private async runBatchSim() {
		if (this.isRunning) return;

		this.progressTrackerModal.show();

		trackEvent({
			action: 'sim',
			category: 'simulate',
			label: 'batch',
			value: this.combinations,
		});

		this.isRunning = true;
		this.isCancelling = false;
		const concurrency = (await this.simUI.sim.shouldUseWasmConcurrency()) ? this.simUI.sim.getWasmConcurrency() : navigator.hardwareConcurrency || 4;
		this.bulkSimAbortController = new AbortController();
		const abortSignal = this.bulkSimAbortController.signal;
		this.bulkSimButton.disabled = true;
		this.topGearResults = null;
		this.originalGearResults = null;

		const reforgedGearSets: Gear[] = [];

		try {
			await this.simUI.sim.signalManager.abortType(RequestTypes.All);
			this.simStart = new Date().getTime();
			this.originalGear = this.simUI.player.getGear();
			let topGearResults: TopGearResult[] = [];

			this.resetResultsTabContent();
			this.calculateBulkCombinations();

			// [foundation step 2 validation] Confirm the combo-space dimensions we'll hand the
			// backend reproduce the exact combination count the browser computes. No backend call
			// yet - this just proves the dimension-building is faithful before we wire the runner.
			try {
				const comboDimensions = this.buildComboDimensions();
				const dimensionsProduct = comboDimensions.reduce((product, dim) => product * dim.choices.length, 1);
				console.log(
					`[bulk] combo dimensions: ${comboDimensions.length} dims, product = ${dimensionsProduct}, ` +
						`browser combinations = ${this.combinations}, match = ${dimensionsProduct === this.combinations}`,
				);
			} catch (e) {
				console.error('[bulk] buildComboDimensions failed:', e);
			}

			const originalGear = this.originalGear!;

			// Native path: hand the whole batch to the Go backend - it expands the combinations,
			// sims them across all cores, and returns the ranked results. The browser does no
			// per-combo work. (Wasm has no native runner, so it falls through to the per-combo path
			// below. Gemming is still crude here; the cap-aware optimizer port is the next phase.)
			if (!(await this.simUI.sim.isWasm())) {
				const iterations = this.getComboIterations();
				const base = this.simUI.sim.buildGearSimRequest(originalGear, iterations);

				// The combos reference the bulk items, so the base request's database must contain
				// them (the backend resolves item stats from it).
				const baseDb = base.raid!.parties[0].players[0].database!;
				const bulkDb = this.createBulkItemsDatabase();
				baseDb.items.push(...bulkDb.items);
				baseDb.enchants.push(...bulkDb.enchants);
				baseDb.gems.push(...bulkDb.gems);
				baseDb.randomSuffixes.push(...bulkDb.randomSuffixes);
				baseDb.itemEffectRandPropPoints.push(...bulkDb.itemEffectRandPropPoints);

				const request = BulkComboSimRequest.create({
					base,
					dimensions: this.buildComboDimensions(),
					settings: this.createBulkSettings(),
				});

				this.setBatchSimProgress(0, this.combinations);
				const comboResult = await this.runWithBulkAbort(
					this.simUI.sim.runBulkComboSim(request, (progress: ProgressMetrics) =>
						this.setBatchSimProgress(progress.completedSims, progress.totalSims || this.combinations),
					),
					abortSignal,
				);

				// [validation] surface the numbers so they can be checked against a manual single-sim.
				console.log(
					`[bulk] native runner: ${comboResult.totalCombinations} combos, baseline dps = ${comboResult.baseline?.dps?.avg?.toFixed(1)}, ` +
						`top = [${comboResult.ranked.map(r => (r.dps?.avg ?? 0).toFixed(1)).join(', ')}]`,
				);

				this.topGearResults = comboResult.ranked.map(r => ({
					gear: this.simUI.sim.db.lookupEquipmentSpec(r.equipment!),
					dpsMetrics: r.dps!,
				}));
				if (comboResult.baseline) {
					this.originalGearResults = {
						gear: this.simUI.sim.db.lookupEquipmentSpec(comboResult.baseline.equipment!),
						dpsMetrics: comboResult.baseline.dps!,
					};
					this.topGearResults.push(this.originalGearResults);
				}
				this.topGearResults.sort((a, b) => b.dpsMetrics.avg - a.dpsMetrics.avg);
				this.buildResultsTabContent();
				return;
			}

			const defaultGemsByColor = new Map<GemColor, UIGem | null>();

			for (const [colorIdx, color] of [
				GemColor.GemColorRed,
				GemColor.GemColorYellow,
				GemColor.GemColorBlue,
				GemColor.GemColorMeta,
				GemColor.GemColorPrismatic,
			].entries()) {
				defaultGemsByColor.set(color, this.simUI.sim.db.lookupGem(this.fallbackGems[colorIdx].id));
			}

			// Build a single combination's gear set on demand. The old code materialized
			// every combination (item maps + candidate gear sets + reforged gear sets) up
			// front, which is what made the tab run out of memory and crash on large batches.
			const buildCandidateGear = (comboIdx: number): Gear => {
				let reforgeGear = originalGear;

				for (const [itemSlot, equippedItem] of this.getItemsForCombo(comboIdx).entries()) {
					const equippedItemInSlot = originalGear.getEquippedItem(itemSlot);
					let updatedItem = equippedItemInSlot ? equippedItemInSlot.withItem(equippedItem.item) : equippedItem;

					if (equippedItem._randomSuffix) {
						updatedItem = updatedItem.withRandomSuffix(equippedItem._randomSuffix);
					}

					reforgeGear = reforgeGear.withEquippedItem(itemSlot, updatedItem);

					for (const [socketIdx, socketColor] of equippedItem.curSocketColors().entries()) {
						if (defaultGemsByColor.get(socketColor)) {
							reforgeGear = reforgeGear.withGem(itemSlot, socketIdx, defaultGemsByColor.get(socketColor)!);
						}
					}
				}

				return reforgeGear;
			};

			if (this.optimizeGems && this.simUI.reforger) {
				let completedReforges = 1;
				this.setReforgeProgress(completedReforges, this.combinations);
				await sleep(400);
				const reforgeTasks = Array.from({ length: this.combinations }, (_, comboIdx) => async () => {
					this.throwIfBulkAborted(abortSignal);
					const reforgedGear = await this.optimizeReforges(buildCandidateGear(comboIdx), abortSignal);
					this.throwIfBulkAborted(abortSignal);
					completedReforges += 1;
					this.setReforgeProgress(completedReforges, this.combinations);
					return reforgedGear;
				});
				const reforgeSettledResults = await runWithConcurrency(reforgeTasks, concurrency);
				const rejectedReforge = reforgeSettledResults.find(result => result.status === 'rejected');
				if (rejectedReforge && rejectedReforge.status === 'rejected') {
					throw rejectedReforge.reason;
				}
				const reforgeResults = reforgeSettledResults
					.filter((result): result is PromiseFulfilledResult<Gear | null> => result.status === 'fulfilled')
					.map(result => result.value);

				reforgedGearSets.push(...reforgeResults.filter((gear): gear is Gear => !!gear));
			}
			// When gem optimization is off, reforgedGearSets stays empty and each combo's gear is
			// built lazily per chunk below, so we never materialize every combination up front.

			this.simStart = new Date().getTime();
			const iterations = this.getComboIterations();
			const haveOptimizedGear = this.optimizeGems && !!this.simUI.reforger;
			const comboCount = haveOptimizedGear ? reforgedGearSets.length : this.combinations;
			const getGear = (i: number): Gear => (haveOptimizedGear ? reforgedGearSets[i] : buildCandidateGear(i));

			this.setBatchSimProgress(0, comboCount);

			// Baseline (current gear) on its own, for the reference DPS.
			const baseResults = await this.runWithBulkAbort(
				this.simUI.sim.runBulkSim([this.simUI.sim.buildGearSimRequest(originalGear, iterations)], () => {}),
				abortSignal,
			);
			const referenceDpsMetrics = baseResults[0]!.raidMetrics!.dps!;

			// Sim the combinations in chunks. Each chunk is one bulk request the server runs across
			// all cores; chunking keeps memory bounded (only one chunk's requests + results exist at a
			// time) and the UI responsive (we yield between chunks). Running everything in a single
			// request previously ballooned to tens of GB and produced a result too large to send back.
			const CHUNK_SIZE = 200;
			let completed = 0;
			for (let start = 0; start < comboCount; start += CHUNK_SIZE) {
				this.throwIfBulkAborted(abortSignal);

				const end = Math.min(start + CHUNK_SIZE, comboCount);
				const chunkGears: Gear[] = [];
				const requests: RaidSimRequest[] = [];
				for (let i = start; i < end; i++) {
					const gear = getGear(i);
					chunkGears.push(gear);
					requests.push(this.simUI.sim.buildGearSimRequest(gear, iterations));
				}

				const chunkResults = await this.runWithBulkAbort(
					this.simUI.sim.runBulkSim(requests, (progress: ProgressMetrics) => this.setBatchSimProgress(completed + progress.completedSims, comboCount)),
					abortSignal,
				);

				for (let i = 0; i < chunkGears.length; i++) {
					const reforgedGear = chunkGears[i];
					const result = chunkResults[i];
					if (!result || result.error || originalGear.equals(reforgedGear)) {
						continue;
					}

					const dpsMetrics = result.raidMetrics!.dps!;
					dpsMetrics.hist = [];
					dpsMetrics.allValues = [];
					topGearResults.push({ gear: reforgedGear, dpsMetrics });
					topGearResults.sort((a, b) => b.dpsMetrics.avg - a.dpsMetrics.avg);
					if (topGearResults.length > 5) topGearResults.pop();
				}

				completed += chunkGears.length;
				this.setBatchSimProgress(completed, comboCount);
				await sleep(0); // yield so the UI stays responsive between chunks
			}

			this.topGearResults = topGearResults;
			this.originalGearResults = {
				gear: this.originalGear,
				dpsMetrics: referenceDpsMetrics,
			};

			this.topGearResults.push(this.originalGearResults);
			this.topGearResults.sort((a, b) => b.dpsMetrics.avg - a.dpsMetrics.avg);

			this.buildResultsTabContent();
		} catch (error) {
			console.error(error);
			if (!this.isCancelling && typeof error === 'string') {
				new Toast({
					variant: 'error',
					body: error,
				});
			}
		} finally {
			await this.simUI.player.setGearAsync(TypedEvent.nextEventID(), this.originalGear!);
			this.bulkSimButton.disabled = false;
			if (this.isCancelling) {
				new Toast({
					variant: 'error',
					body: i18n.t('bulk_tab.notifications.bulk_sim_cancelled'),
				});
			}
			this.isRunning = false;
			this.isCancelling = false;
			this.progressTrackerModal.hide();
		}
	}

	// Progress for the concurrent batch run, driven by how many combinations have finished
	// (per-iteration progress is meaningless when several sims run at once).
	private setBatchSimProgress(completed: number, total: number) {
		const totalElapsedSeconds = (new Date().getTime() - this.simStart) / 1000;
		const secondsRemaining = completed > 0 ? (totalElapsedSeconds / completed) * (total - completed) : 0;

		this.progressTrackerModal.updateProgress({
			stage: 'sim',
			title: i18n.t('bulk_tab.progress.refining_rounds'),
			current: completed,
			total: total,
			message: (
				<div className="results-sim">
					<div>{`${completed} / ${total} combinations simulated`}</div>
					<div>{i18n.t('bulk_tab.progress.seconds_remaining', { time: formatDuration(secondsRemaining) })}</div>
				</div>
			),
		});
	}

	private async runSingleGearSim(gear: Gear): Promise<RaidSimResult> {
		// Call sim.runRaidSimLightweight directly instead of simUI.runSimLightweight: the latter
		// aborts ALL running sims on entry, which would make concurrent batch sims kill each other.
		// The batch already does a single abortType(All) at the start to clear any prior runs.
		const response = await this.simUI.sim.runRaidSimLightweight(gear, () => {}, { iterations: this.getComboIterations() });
		if (!response || (response && 'type' in response)) {
			throw new Error(response?.message);
		}

		const [_, result] = response;

		return result;
	}

	private async optimizeReforges(gear: Gear, signal: AbortSignal): Promise<Gear | null> {
		if (!this.simUI.reforger) {
			return gear;
		}

		this.throwIfBulkAborted(signal);

		try {
			return this.runWithBulkAbort(this.simUI.reforger.optimizeReforges(gear, true), signal);
		} catch {
			this.throwIfBulkAborted(signal);

			try {
				return this.runWithBulkAbort(this.simUI.reforger.optimizeReforges(gear, true), signal);
			} catch {
				this.throwIfBulkAborted(signal);
				return gear;
			}
		}
	}

	private async abortBulkSim() {
		if (this.isCancelling) return;

		try {
			this.isCancelling = true;
			await Promise.all([this.simUI.reforger?.abortReforgeOptimization(), this.simUI.sim.signalManager.abortType(RequestTypes.All)]);
			if (!this.bulkSimAbortController?.signal.aborted) {
				this.bulkSimAbortController?.abort();
				this.bulkSimAbortController = null;
			}
		} finally {
			this.bulkSimButton.disabled = false;
		}
	}

	private throwIfBulkAborted(signal: AbortSignal) {
		if (signal.aborted || this.isCancelling) {
			throw new Error('Bulk Sim Aborted');
		}
	}

	private async runWithBulkAbort<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
		this.throwIfBulkAborted(signal);

		let abortHandler: (() => void) | null = null;
		const abortPromise = new Promise<never>((_, reject) => {
			abortHandler = () => reject(new Error('Bulk Sim Aborted'));
			signal.addEventListener('abort', abortHandler, { once: true });
		});

		try {
			return Promise.race([promise, abortPromise]);
		} finally {
			if (abortHandler) {
				signal.removeEventListener('abort', abortHandler);
			}
		}
	}
}
