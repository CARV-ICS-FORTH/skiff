import unittest

import numpy as np
from gymnasium.utils.env_checker import check_env

from retail.demand import demand_series, feature, make_frame
from retail.inventory import InventoryEnv, evaluate_policy


class DemandTests(unittest.TestCase):
    def test_series_are_independent_of_generation_partition(self):
        whole = make_frame(37, [0, 1, 2], 80, 50, 65)
        separate = make_frame(37, [2], 80, 50, 65)
        np.testing.assert_array_equal(
            whole[whole.series_id == 2].to_numpy(), separate.to_numpy()
        )

    def test_future_demand_cannot_change_a_feature(self):
        demand, promotion, base = demand_series(7, 1, 100)
        before = feature(demand[:40], 40, promotion[40], base)
        demand[40:] += 10000
        np.testing.assert_array_equal(
            before, feature(demand[:40], 40, promotion[40], base)
        )

    def test_chronological_splits_and_seasonal_baseline(self):
        frame = make_frame(37, [0], 80, 50, 65)
        self.assertEqual(
            frame.groupby("split").size().to_dict(),
            {"train": 22, "validation": 15, "test": 15},
        )
        demand, _, _ = demand_series(37, 0, 80)
        self.assertAlmostEqual(frame.iloc[0]["seasonal"] * 100, demand[21], places=5)


class InventoryTests(unittest.TestCase):
    def config(self):
        return {"seed": 1, "series": 10, "horizon": 28}

    def test_gym_contract(self):
        check_env(InventoryEnv(self.config()), skip_render_check=True)

    def test_orders_arrive_after_three_steps_and_costs_balance(self):
        env = InventoryEnv(self.config())
        env.reset(seed=1)
        env.stock = 0
        env.demand[:] = 0
        _, reward, _, _, first = env.step(2)
        self.assertEqual(first["delivered"], 0)
        self.assertEqual(first["order"], env.base)
        self.assertEqual(reward, -2 / env.base)
        env.step(0)
        env.step(0)
        _, _, _, _, fourth = env.step(0)
        self.assertEqual(fourth["delivered"], env.base)
        self.assertEqual(fourth["stock"], env.base)
        self.assertAlmostEqual(env.total_cost, 2 + env.base * 0.1)

    def test_demand_is_conserved_and_no_inventory_is_negative(self):
        env = InventoryEnv(self.config())
        env.reset(seed=88)
        for _ in range(28):
            _, _, _, _, info = env.step(0)
            self.assertGreaterEqual(info["stock"], 0)
            self.assertAlmostEqual(info["sales"] + info["lost_units"], info["demand"])

    def test_policy_evaluation_replays_identical_paths(self):
        a = evaluate_policy(self.config(), "forecast_base_stock", [10, 11, 12])
        b = evaluate_policy(self.config(), "forecast_base_stock", [10, 11, 12])
        self.assertEqual(a, b)
        self.assertGreater(a["mean_cost"], 0)
        self.assertTrue(0 <= a["fill_rate"] <= 1)

    def test_out_of_range_action_is_rejected(self):
        env = InventoryEnv(self.config())
        with self.assertRaises(ValueError):
            env.step(7)
