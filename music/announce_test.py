import unittest

import announce


class AnnounceTextTest(unittest.TestCase):
    def test_default_sentence(self):
        self.assertEqual(
            announce.render_announce("", "张三"),
            "收到来自张三的通话，即将自动接听",
        )

    def test_custom_template(self):
        self.assertEqual(
            announce.render_announce("来电：{name}", "alice"),
            "来电：alice",
        )

    def test_strips_newlines_and_limits_length(self):
        text = "收到\n来自{name}" + "啊" * 100
        cleaned = announce.clean_template(text)
        self.assertNotIn("\n", cleaned)
        self.assertLessEqual(len(cleaned), announce.MAX_TEXT)

    def test_empty_name_fallback(self):
        self.assertEqual(announce.clean_name(" \n{}"), "有人")

    def test_name_length(self):
        self.assertEqual(len(announce.clean_name("名" * 40)), announce.MAX_NAME)

    def test_unknown_sound_falls_back(self):
        self.assertEqual(announce.clean_sound("nope"), "single")
        self.assertEqual(announce.clean_sound("phone"), "phone")


if __name__ == "__main__":
    unittest.main()